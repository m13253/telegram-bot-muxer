package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const UserAgent = "Mozilla/5.0 Telegram-bot-muxer/1.0 (+https://github.com/m13253/telegram-bot-muxer)"

type Client struct {
	conf              *Config
	db                *Database
	typesNeedCaching  map[string]struct{}
	echoProcessor     map[string]func([]byte)
	nextRetryInterval time.Duration
	cooldownMutex     *sync.RWMutex
	globalCooldown    time.Time
	chatCooldown      map[int64]time.Time
}

func NewClient(conf *Config, db *Database) *Client {
	c := &Client{
		conf: conf,
		db:   db,
		typesNeedCaching: map[string]struct{}{
			"message":                 {},
			"edited_message":          {},
			"channel_post":            {},
			"edited_channel_post":     {},
			"business_message":        {},
			"edited_business_message": {},
		},
		nextRetryInterval: time.Second,
		cooldownMutex:     new(sync.RWMutex),
		globalCooldown:    time.Now(),
		chatCooldown:      make(map[int64]time.Time),
	}
	c.echoProcessor = map[string]func([]byte){
		"sendMessage":             c.processEchoMessage,
		"forwardMessage":          c.processEchoMessage,
		"copyMessage":             c.processEchoMessage,
		"sendPhoto":               c.processEchoMessage,
		"sendAudio":               c.processEchoMessage,
		"sendDocument":            c.processEchoMessage,
		"sendVideo":               c.processEchoMessage,
		"sendAnimation":           c.processEchoMessage,
		"sendVoice":               c.processEchoMessage,
		"sendVideoNote":           c.processEchoMessage,
		"sendPaidMedia":           c.processEchoMessage,
		"sendMediaGroup":          c.processEchoMessageArray,
		"sendLocation":            c.processEchoMessage,
		"sendVenue":               c.processEchoMessage,
		"sendContact":             c.processEchoMessage,
		"sendPoll":                c.processEchoMessage,
		"sendDice":                c.processEchoMessage,
		"editMessageText":         c.processEchoMessageEdit,
		"editMessageCaption":      c.processEchoMessageEdit,
		"editMessageMedia":        c.processEchoMessageEdit,
		"editMessageLiveLocation": c.processEchoMessageEdit,
		"stopMessageLiveLocation": c.processEchoMessageEdit,
		"editMessageReplyMarkup":  c.processEchoMessageEdit,
	}
	return c
}

func (c *Client) StartPolling(ctx context.Context) error {
	offset := uint64(0)

	for {
		var requestURL string
		if offset == 0 {
			requestURL = fmt.Sprintf(
				"%s/getUpdates?timeout=%d&allowed_updates=%s",
				c.conf.Upstream.ApiPrefix, c.conf.Upstream.PollingTimeout, c.conf.Upstream.FilterUpdateTypesStr,
			)
		} else {
			requestURL = fmt.Sprintf(
				"%s/getUpdates?offset=%d&timeout=%d&allowed_updates=%s",
				c.conf.Upstream.ApiPrefix, offset, c.conf.Upstream.PollingTimeout, c.conf.Upstream.FilterUpdateTypesStr,
			)
		}
		log.Println("GET", requestURL)

		req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
		if err != nil {
			return fmt.Errorf("failed to send HTTP request: %v", err)
		}
		req.Header.Set("User-Agent", UserAgent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Assume this is not a fatal error
			log.Println("Upstream HTTP request error:", err)
			c.sleepUntilRetry()
			continue
		}
		defer resp.Body.Close()

		requestSucceed := resp.StatusCode >= 200 && resp.StatusCode < 300
		failureIsFatal := resp.StatusCode >= 400 && resp.StatusCode < 500
		if !requestSucceed {
			log.Println("Upstream server returned error:", resp.Status)
		}
		if failureIsFatal {
			return fmt.Errorf("HTTP error: %s", resp.Status)
		}
		if !requestSucceed {
			c.sleepUntilRetry()
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Println("HTTP read error:", err)
			c.sleepUntilRetry()
			continue
		}

		bodyJson := gjson.ParseBytes(body)
		if bodyJson.Get("ok").Type != gjson.True {
			errorCode := bodyJson.Get("error_code").String()
			errorDesc := bodyJson.Get("description").String()
			log.Println("Upstream error:", errorCode, errorDesc)
			c.sleepUntilRetry()
			continue
		}

		tx, err := c.db.BeginTx()
		if err != nil {
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		bodyJson.Get("result").ForEach(func(_, update gjson.Result) bool {
			upstreamID := update.Get("update_id").Uint()
			offset = max(offset, upstreamID+1)
			update.ForEach(func(updateType, updateValue gjson.Result) bool {
				if updateType.Str == "update_id" {
					return true
				}
				if _, ok := c.typesNeedCaching[updateType.Str]; ok {
					err = tx.InsertMessage(&updateValue)
					if err != nil {
						return false
					}
				}
				err = tx.InsertUpdate(upstreamID, updateType.String(), updateValue.Raw)
				return err == nil
			})
			return err == nil
		})
		if err != nil {
			tx.Commit()
			c.db.NotifyUpdates()
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}
		err = tx.Commit()
		c.db.NotifyUpdates()
		if err != nil {
			log.Println("Failed to store updates:", err)
			c.sleepUntilRetry()
			continue
		}

		c.resetRetry()
	}
}

func (c *Client) ForwardRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, prefix string, suffix string, isFile bool) error {
	var requestURL string
	if len(r.URL.RawQuery) == 0 {
		requestURL = fmt.Sprintf("%s/%s", prefix, suffix)
	} else {
		requestURL = fmt.Sprintf("%s/%s?%s", prefix, suffix, r.URL.RawQuery)
	}
	log.Println(r.Method, requestURL)

	if !isFile {
		chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
		if chatID != 0 {
			c.cooldownMutex.RLock()
			cooldown := c.globalCooldown
			if cd, ok := c.chatCooldown[chatID]; ok && cd.After(cooldown) {
				cooldown = cd
			}
			c.cooldownMutex.RUnlock()
			sleep := time.Until(cooldown)
			if sleep > 0 {
				select {
				case <-ctx.Done():
					return context.Canceled
				case <-time.After(sleep):
				}
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, requestURL, r.Body)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %v", err)
	}
	for k, v := range r.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Host" && k != "Proxy-Connection" && k != "User-Agent" {
			req.Header[k] = v
		}
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream HTTP request error: %v", err)
	}
	defer resp.Body.Close()

	respHeader := w.Header()
	for k, v := range resp.Header {
		if k != "Accept-Encoding" && k != "Content-Encoding" && k != "Connection" && k != "Proxy-Connection" {
			respHeader[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Too late to report error, so ignore errors from here

	var echoProcessor func([]byte)
	if !isFile {
		echoProcessor = c.echoProcessor[suffix]
	}
	if echoProcessor == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			debug.PrintStack()
			log.Println("HTTP error:", err)
		}
		return nil
	}

	var bodyCopy bytes.Buffer
	_, err = io.Copy(w, io.TeeReader(resp.Body, &bodyCopy))
	if err != nil {
		debug.PrintStack()
		log.Println("HTTP error:", err)
		return nil
	}

	echoProcessor(bodyCopy.Bytes())
	return nil
}

func (c *Client) sleepUntilRetry() {
	time.Sleep(c.nextRetryInterval)
	c.nextRetryInterval = min(c.nextRetryInterval*2, time.Duration(c.conf.Upstream.MaxRetryInterval)*time.Second)
}

func (c *Client) resetRetry() {
	c.nextRetryInterval = time.Second
}

func (c *Client) processEchoMessage(body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	message := bodyJson.Get("result")
	c.updateRateLimit(&message)
	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertMessage(&message)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertLocalUpdate("message", message.Raw)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}

func (c *Client) processEchoMessageEdit(body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	message := bodyJson.Get("result")
	if message.Type == gjson.True {
		return
	}
	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertMessage(&message)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.InsertLocalUpdate("edited_message", message.Raw)
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}

func (c *Client) processEchoMessageArray(body []byte) {
	bodyJson := gjson.ParseBytes(body)
	if bodyJson.Get("ok").Type != gjson.True {
		errorCode := bodyJson.Get("error_code").String()
		errorDesc := bodyJson.Get("description").String()
		log.Println("Upstream error:", errorCode, errorDesc)
		return
	}

	tx, err := c.db.BeginTx()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	bodyJson.Get("result").ForEach(func(_, message gjson.Result) bool {
		c.updateRateLimit(&message)
		err := tx.InsertMessage(&message)
		if err != nil {
			log.Println("Failed to store updates:", err)
		}
		err = tx.InsertLocalUpdate("message", message.Raw)
		if err != nil {
			log.Println("Failed to store updates:", err)
		}
		return true
	})
	err = tx.Commit()
	if err != nil {
		log.Println("Failed to store updates:", err)
	}
	c.db.NotifyUpdates()
}

func (c *Client) updateRateLimit(message *gjson.Result) {
	// https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this

	now := time.Now()
	c.cooldownMutex.Lock()
	c.globalCooldown = now.Add(time.Second/30 + 1)

	chatID := message.Get("chat.id").Int()
	if chatID == 0 {
		c.cooldownMutex.Unlock()
		return
	}
	chatType := message.Get("chat.type").String()
	if chatType == "private" {
		c.chatCooldown[chatID] = now.Add(time.Second)
	} else {
		c.chatCooldown[chatID] = now.Add(3 * time.Second)
	}
	c.cooldownMutex.Unlock()
}

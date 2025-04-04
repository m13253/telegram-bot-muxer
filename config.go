package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DB         string           `toml:"db"`
	Upstream   ConfigUpstream   `toml:"upstream"`
	Downstream ConfigDownstream `toml:"downstream"`
}

type ConfigUpstream struct {
	ApiUrl               string   `toml:"api_url"`
	FileUrl              string   `toml:"file_url"`
	AuthToken            string   `toml:"auth_token"`
	PollingTimeout       uint64   `toml:"polling_timeout"`
	MaxRetryInterval     uint64   `toml:"max_retry_interval"`
	FilterUpdateTypes    []string `toml:"filter_update_types"`
	ApiPrefix            string   `toml:"-"`
	FilePrefix           string   `toml:"-"`
	FilterUpdateTypesStr string   `toml:"-"`
}

type ConfigDownstream struct {
	ListenAddr string   `toml:"listen_addr"`
	ApiPath    string   `toml:"api_path"`
	FilePath   string   `toml:"file_path"`
	AuthToken  string   `toml:"auth_token"`
	ApiPrefix  []string `toml:"-"`
	FilePrefix []string `toml:"-"`
}

func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load config file: %v", err)
	}
	d := toml.NewDecoder(file)
	conf := &Config{
		DB: "tbmux.db",
		Upstream: ConfigUpstream{
			ApiUrl:            "https://api.telegram.org/bot",
			FileUrl:           "https://api.telegram.org/file/bot",
			PollingTimeout:    60,
			MaxRetryInterval:  600,
			FilterUpdateTypes: []string{},
		},
		Downstream: ConfigDownstream{
			ApiPath:  "/bot",
			FilePath: "/file/bot",
		},
	}
	_, err = d.Decode(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to load config file: %v", err)
	}

	// Check for errors
	if len(conf.DB) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "db"}
	}
	if len(conf.Upstream.ApiUrl) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "upstream.api_url"}
	}
	if len(conf.Upstream.FileUrl) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "upstream.file_url"}
	}
	if len(conf.Upstream.AuthToken) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "upstream.auth_token"}
	}
	if conf.Upstream.PollingTimeout < 10 {
		return nil, &errConfigDurationIsTooShort{field: "upstream.polling_timeout"}
	}
	if conf.Upstream.MaxRetryInterval < 60 {
		return nil, &errConfigDurationIsTooShort{field: "upstream.max_retry_interval"}
	}
	if len(conf.Downstream.ListenAddr) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "downstream.listen_addr"}
	}
	if len(conf.Downstream.ApiPath) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "downstream.api_path"}
	}
	if len(conf.Downstream.FilePath) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "downstream.file_path"}
	}
	if len(conf.Downstream.AuthToken) == 0 {
		return nil, &errConfigFieldIsEmpty{field: "downstream.auth_token"}
	}

	// Join prefixes
	conf.Upstream.ApiPrefix = conf.Upstream.ApiUrl + url.PathEscape(conf.Upstream.AuthToken)
	conf.Upstream.FilePrefix = conf.Upstream.FileUrl + url.PathEscape(conf.Upstream.AuthToken)

	// Convert FilterUpdateTypes to string
	filterUpdateTypesBuf, err := json.Marshal(conf.Upstream.FilterUpdateTypes)
	if err != nil {
		return nil, fmt.Errorf("invalid config file: upstream.filter_update_types is invalid: %v", err)
	}
	conf.Upstream.FilterUpdateTypesStr = url.QueryEscape(string(filterUpdateTypesBuf))

	// Split prefixes
	apiPrefix, err := url.ParseRequestURI(conf.Downstream.ApiPath)
	if err != nil {
		return nil, fmt.Errorf("invalid config file: downstream.api_path is invalid: %v", err)
	}
	conf.Downstream.ApiPrefix = strings.Split(apiPrefix.EscapedPath(), "/")
	for i := range conf.Downstream.ApiPrefix {
		conf.Downstream.ApiPrefix[i], err = url.PathUnescape(conf.Downstream.ApiPrefix[i])
		if err != nil {
			return nil, fmt.Errorf("invalid config file: downstream.api_path is invalid: %v", err)
		}
	}

	filePrefix, err := url.ParseRequestURI(conf.Downstream.FilePath)
	if err != nil {
		return nil, fmt.Errorf("invalid config file: downstream.file_path is invalid: %v", err)
	}
	conf.Downstream.FilePrefix = strings.Split(filePrefix.EscapedPath(), "/")
	for i := range conf.Downstream.FilePrefix {
		conf.Downstream.FilePrefix[i], err = url.PathUnescape(conf.Downstream.FilePrefix[i])
		if err != nil {
			return nil, fmt.Errorf("invalid config file: downstream.file_path is invalid: %v", err)
		}
	}
	return conf, nil
}

type errConfigFieldIsEmpty struct {
	field string
}

func (e *errConfigFieldIsEmpty) Error() string {
	return fmt.Sprintf("invalid config file: %s is empty", e.field)
}

type errConfigDurationIsTooShort struct {
	field string
}

func (e *errConfigDurationIsTooShort) Error() string {
	return fmt.Sprintf("invalid config file: %s is too short", e.field)
}

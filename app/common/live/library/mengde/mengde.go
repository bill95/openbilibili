package mengde

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-common/library/log"
)

const (
	kSvenCheckTimeout = time.Minute
	kSvenGetTimeout   = 30 * time.Second
	kSvenHost         = "http://config.bilibili.co"
	kSvenCheckAPI     = kSvenHost + "/config/v2/check"
	kSvenGetAPI       = kSvenHost + "/config/v2/get"
	kDefaultHost      = "invalid-host-name"
	kDefaultIP        = "127.0.0.1"
	kCodeNotModified  = -304
)

type MengdeClient struct {
	treeID         string
	zone           string
	env            string
	build          string
	token          string
	host           string
	ip             string
	config         atomic.Value
	notify         chan *MengdeConfig
	changeSignal   chan struct{}
	httpClient     *http.Client
	ctx            context.Context
	cancel         context.CancelFunc
	closeSignal    chan struct{}
	configLoadWg   sync.WaitGroup
	configNotifyWg sync.WaitGroup
}

type MengdeConfig struct {
	Version int
	Config  map[string]string
}

func NewMengdeClient(treeID string, zone string, env string, build string, token string) (*MengdeClient, error) {
	host, err := os.Hostname()
	if err != nil {
		host = kDefaultHost
	}
	ip := kDefaultIP
	addr, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addr {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
				ip = n.IP.String()
			}
		}
	}
	c := &MengdeClient{
		treeID:       treeID,
		zone:         zone,
		env:          env,
		build:        build,
		token:        token,
		host:         host,
		ip:           ip,
		notify:       make(chan *MengdeConfig),
		changeSignal: make(chan struct{}, 1),
		httpClient:   new(http.Client),
		closeSignal:  make(chan struct{}),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	version, config, err := c.svenConfigSync()
	if err != nil {
		return nil, err
	}
	c.config.Store(&MengdeConfig{
		Version: version,
		Config:  config,
	})
	c.changeSignal <- struct{}{}

	c.configLoadWg.Add(1)
	go func() {
		defer c.configLoadWg.Done()
		c.configLoadProcess()
	}()

	c.configNotifyWg.Add(1)
	go func() {
		defer c.configNotifyWg.Done()
		c.configNotifyProcess()
	}()
	return c, nil
}

func (c *MengdeClient) Close() {
	close(c.closeSignal)
	c.cancel()
	c.configLoadWg.Wait()

	close(c.changeSignal)
	c.configNotifyWg.Wait()
}

func (c *MengdeClient) Config() *MengdeConfig {
	result := &MengdeConfig{
		Version: -1,
		Config:  make(map[string]string),
	}
	if config, ok := c.config.Load().(*MengdeConfig); ok {
		result.Version = config.Version
		for k, v := range config.Config {
			result.Config[k] = v
		}
	}
	return result
}

func (c *MengdeClient) ConfigNotify() <-chan *MengdeConfig {
	return c.notify
}

func (c *MengdeClient) configLoadProcess() {
	for {
		select {
		case <-c.closeSignal:
			return
		default:
		}
		current, ok := c.config.Load().(*MengdeConfig)
		if !ok {
			current = &MengdeConfig{
				Version: -1,
			}
		}
		version, err := c.svenCheckVersion(current.Version)
		//config not modified
		if version == current.Version {
			log.Info("[sven]check version config not modified")
			continue
		}
		if err != nil {
			log.Error("[sven]check version error:%s", err.Error())
			continue
		}
		if current.Version == version {
			continue
		}
		config, err := c.svenGetConfig(version)
		if err != nil {
			log.Error(err.Error())
			continue
		}
		c.config.Store(&MengdeConfig{
			Version: version,
			Config:  config,
		})
		select {
		case c.changeSignal <- struct{}{}:
		default:
		}
	}
}

func (c *MengdeClient) configNotifyProcess() {
	for range c.changeSignal {
		select {
		case <-c.closeSignal:
			return
		case c.notify <- c.Config():
		}
	}
}

func (c *MengdeClient) svenConfigSync() (int, map[string]string, error) {
	version, err := c.svenCheckVersion(-1)
	if err != nil {
		return -1, nil, err
	}
	config, err := c.svenGetConfig(version)
	if err != nil {
		return -1, nil, err
	}
	return version, config, nil
}

func (c *MengdeClient) svenCheckVersion(version int) (int, error) {
	var err error
	req, err := http.NewRequest("GET", kSvenCheckAPI, nil)
	if err != nil {
		return -1, err
	}
	q := req.URL.Query()
	q.Add("build", c.build)
	q.Add("hostname", c.host)
	q.Add("ip", c.ip)
	q.Add("service", strings.Join([]string{c.treeID, c.env, c.zone}, "_"))
	q.Add("token", c.token)
	q.Add("version", strconv.Itoa(version))
	req.URL.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(c.ctx, kSvenCheckTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, errors.New(fmt.Sprintf("http request URL:%s result:%s", req.URL.RawQuery, resp.Status))
	}
	result, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return -1, err
	}
	var svenCheckRespBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err = json.Unmarshal(result, &svenCheckRespBody); err != nil {
		return -1, err
	}
	if svenCheckRespBody.Code != 0 {
		if svenCheckRespBody.Code == kCodeNotModified {
			return version, nil
		}
		return -1, errors.New(svenCheckRespBody.Message)
	}
	return svenCheckRespBody.Data.Version, nil
}

func (c *MengdeClient) svenGetConfig(version int) (map[string]string, error) {
	var err error
	req, err := http.NewRequest("GET", kSvenGetAPI, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()

	q.Add("build", c.build)
	q.Add("hostname", c.host)
	q.Add("ip", c.ip)
	q.Add("service", strings.Join([]string{c.treeID, c.env, c.zone}, "_"))
	q.Add("token", c.token)
	q.Add("version", strconv.Itoa(version))
	req.URL.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(c.ctx, kSvenGetTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("http request URL:%s result:%s", req.URL.RawQuery, resp.Status))
	}
	result, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var svenGetRespBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		TTL     int    `json:"ttl"`
		Data    struct {
			Version int    `json:"version"`
			MD5     string `json:"md5"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err = json.Unmarshal(result, &svenGetRespBody); err != nil {
		return nil, err
	}
	if svenGetRespBody.Code != 0 {
		return nil, errors.New(svenGetRespBody.Message)
	}
	var configContent []struct {
		Name   string `json:"name"`
		Config string `json:"config"`
	}
	if err = json.Unmarshal([]byte(svenGetRespBody.Data.Content), &configContent); err != nil {
		return nil, err
	}
	config := make(map[string]string)
	for _, c := range configContent {
		config[c.Name] = strings.TrimSpace(c.Config)
	}
	return config, nil
}

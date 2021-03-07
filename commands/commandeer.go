// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

import (
	"bytes"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	hconfig "github.com/gohugoio/hugo/config"

	"golang.org/x/sync/semaphore"

	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/common/hugo"

	jww "github.com/spf13/jwalterweatherman"

	"github.com/gohugoio/hugo/common/loggers"
	"github.com/gohugoio/hugo/config"

	"github.com/spf13/cobra"

	"github.com/gohugoio/hugo/hugolib"
	"github.com/spf13/afero"

	"github.com/bep/debounce"
	"github.com/gohugoio/hugo/common/types"
	"github.com/gohugoio/hugo/deps"
	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/langs"
)

type commandeerHugoState struct {
	*deps.DepsCfg
	hugoSites *hugolib.HugoSites
	fsCreate  sync.Once
	created   chan struct{}
}

type commandeer struct {
	*commandeerHugoState

	logger       loggers.Logger
	serverConfig *config.Server

	// Currently only set when in "fast render mode". But it seems to
	// be fast enough that we could maybe just add it for all server modes.
	changeDetector *fileChangeDetector

	// We need to reuse this on server rebuilds.
	destinationFs afero.Fs

	h    *hugoBuilderCommon
	ftch flagsToConfigHandler

	visitedURLs *types.EvictingStringQueue

	cfgInit func(c *commandeer) error

	// We watch these for changes.
	configFiles []string

	// 防抖函数
	// Used in cases where we get flooded with events in server mode.
	debounce func(f func())

	serverPorts         []int
	languagesConfigured bool
	languages           langs.Languages
	doLiveReload        bool
	fastRenderMode      bool
	showErrorInBrowser  bool
	wasError            bool

	configured bool
	paused     bool

	// 同步信号量
	fullRebuildSem *semaphore.Weighted

	// Any error from the last build.
	buildErr error
}

func newCommandeerHugoState() *commandeerHugoState {
	return &commandeerHugoState{
		// 创建标记完成的通道
		created: make(chan struct{}),
	}
}

func (c *commandeerHugoState) hugo() *hugolib.HugoSites {
	// 阻塞到创建完成
	<-c.created
	return c.hugoSites
}

func (c *commandeer) errCount() int {
	return int(c.logger.LogCounters().ErrorCounter.Count())
}

func (c *commandeer) getErrorWithContext() interface{} {
	errCount := c.errCount()

	if errCount == 0 {
		return nil
	}

	m := make(map[string]interface{})

	m["Error"] = errors.New(removeErrorPrefixFromLog(c.logger.Errors()))
	m["Version"] = hugo.BuildVersionString()

	fe := herrors.UnwrapErrorWithFileContext(c.buildErr)
	if fe != nil {
		m["File"] = fe
	}

	if c.h.verbose {
		var b bytes.Buffer
		herrors.FprintStackTraceFromErr(&b, c.buildErr)
		m["StackTrace"] = b.String()
	}

	return m
}

func (c *commandeer) Set(key string, value interface{}) {
	if c.configured {
		panic("commandeer cannot be changed")
	}
	c.Cfg.Set(key, value)
}

func (c *commandeer) initFs(fs *hugofs.Fs) error {
	c.destinationFs = fs.Destination
	c.DepsCfg.Fs = fs

	return nil
}

// 创建编译核心
// running 表示是否开启 watch 模式, 默认为 false
func newCommandeer(mustHaveConfigFile, running bool, h *hugoBuilderCommon, f flagsToConfigHandler, cfgInit func(c *commandeer) error, subCmdVs ...*cobra.Command) (*commandeer, error) {
	var rebuildDebouncer func(f func())
	if running {
		// The time value used is tested with mass content replacements in a fairly big Hugo site.
		// It is better to wait for some seconds in those cases rather than get flooded
		// with rebuilds.
		// 防抖函数
		rebuildDebouncer = debounce.New(4 * time.Second)
	}

	out := ioutil.Discard
	if !h.quiet {
		out = os.Stdout
	}

	c := &commandeer{
		h:                   h,
		ftch:                f, // 获取配置的 interface
		commandeerHugoState: newCommandeerHugoState(), // 创建空的 state
		cfgInit:             cfgInit,
		// 淘汰队列最大为 10
		visitedURLs:         types.NewEvictingStringQueue(10),
		debounce:            rebuildDebouncer, // 默认为 nil
		// 同步信号量，同时只能有一个进行
		fullRebuildSem:      semaphore.NewWeighted(1),
		// This will be replaced later, but we need something to log to before the configuration is read.
		logger: loggers.NewLogger(jww.LevelWarn, jww.LevelError, out, ioutil.Discard, running),
	}

	return c, c.loadConfig(mustHaveConfigFile, running)
}

type fileChangeDetector struct {
	sync.Mutex
	current map[string]string
	prev    map[string]string

	irrelevantRe *regexp.Regexp
}

func (f *fileChangeDetector) OnFileClose(name, md5sum string) {
	f.Lock()
	defer f.Unlock()
	f.current[name] = md5sum
}

func (f *fileChangeDetector) changed() []string {
	if f == nil {
		return nil
	}
	f.Lock()
	defer f.Unlock()
	var c []string
	for k, v := range f.current {
		vv, found := f.prev[k]
		if !found || v != vv {
			c = append(c, k)
		}
	}

	return f.filterIrrelevant(c)
}

func (f *fileChangeDetector) filterIrrelevant(in []string) []string {
	var filtered []string
	for _, v := range in {
		if !f.irrelevantRe.MatchString(v) {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func (f *fileChangeDetector) PrepareNew() {
	if f == nil {
		return
	}

	f.Lock()
	defer f.Unlock()

	if f.current == nil {
		f.current = make(map[string]string)
		f.prev = make(map[string]string)
		return
	}

	f.prev = make(map[string]string)
	for k, v := range f.current {
		f.prev[k] = v
	}
	f.current = make(map[string]string)
}

// loadConfig 初始化配置
func (c *commandeer) loadConfig(mustHaveConfigFile, running bool) error {
	if c.DepsCfg == nil { // 默认为 nil
		c.DepsCfg = &deps.DepsCfg{}
	}

	if c.logger != nil {
		// Truncate the error log if this is a reload.
		c.logger.Reset()
	}

	cfg := c.DepsCfg
	c.configured = false
	cfg.Running = running

	var dir string
	if c.h.source != "" {
		dir, _ = filepath.Abs(c.h.source)
	} else {
		// 设置当前工作文件夹为 source 目录
		dir, _ = os.Getwd()
	}

	// 创建文件操作符
	var sourceFs afero.Fs = hugofs.Os
	if c.DepsCfg.Fs != nil {
		sourceFs = c.DepsCfg.Fs.Source
	}

	// 获取运行环境
	// watch 模式则为 dev, 否则为 prod
	environment := c.h.getEnvironment(running)

	doWithConfig := func(cfg config.Provider) error {
		if c.ftch != nil {
			c.ftch.flagsToConfig(cfg)
		}

		cfg.Set("workingDir", dir)
		cfg.Set("environment", environment)
		return nil
	}

	cfgSetAndInit := func(cfg config.Provider) error {
		c.Cfg = cfg
		if c.cfgInit == nil {
			return nil
		}
		err := c.cfgInit(c)
		return err
	}

	// 配置文件夹目录
	configPath := c.h.source
	if configPath == "" {
		configPath = dir
	}

	// 加载配置到 Viper
	// 返回 viper 对象, 和加载的配置文件名
	config, configFiles, err := hugolib.LoadConfig(
		hugolib.ConfigSourceDescriptor{
			Fs:           sourceFs, // afero.osFs
			Logger:       c.logger,
			Path:         configPath,
			WorkingDir:   dir,
			Filename:     c.h.cfgFile,
			AbsConfigDir: c.h.getConfigDir(dir),
			Environ:      os.Environ(),
			Environment:  environment,
		},
		cfgSetAndInit,
		doWithConfig)

	if err != nil && mustHaveConfigFile {
		return err
	} else if mustHaveConfigFile && len(configFiles) == 0 {
		return hugolib.ErrNoConfigFile
	}

	c.configFiles = configFiles

	// 获取多语言配置
	if l, ok := c.Cfg.Get("languagesSorted").(langs.Languages); ok {
		c.languagesConfigured = true
		c.languages = l
	}

	// Set some commonly used flags
	c.doLiveReload = running && !c.Cfg.GetBool("disableLiveReload")
	c.fastRenderMode = c.doLiveReload && !c.Cfg.GetBool("disableFastRender") // 默认为 false
	c.showErrorInBrowser = c.doLiveReload && !c.Cfg.GetBool("disableBrowserError")

	// This is potentially double work, but we need to do this one more time now
	// that all the languages have been configured.
	if c.cfgInit != nil {
		if err := c.cfgInit(c); err != nil {
			return err
		}
	}

	logger, err := c.createLogger(config, running)
	if err != nil {
		return err
	}

	cfg.Logger = logger
	c.logger = logger
	c.serverConfig, err = hconfig.DecodeServer(cfg.Cfg)
	if err != nil {
		return err
	}

	createMemFs := config.GetBool("renderToMemory")

	if createMemFs {
		// Rendering to memoryFS, publish to Root regardless of publishDir.
		config.Set("publishDir", "/")
	}

	c.fsCreate.Do(func() {
		// 创建 Read Only 文件操作符
		fs := hugofs.NewFrom(sourceFs, config)

		if c.destinationFs != nil {
			// Need to reuse the destination on server rebuilds.
			fs.Destination = c.destinationFs
		} else if createMemFs {
			// Hugo writes the output to memory instead of the disk.
			fs.Destination = new(afero.MemMapFs)
		}

		// 快速渲染模式, 默认为 false
		if c.fastRenderMode {
			// For now, fast render mode only. It should, however, be fast enough
			// for the full variant, too.
			changeDetector := &fileChangeDetector{
				// We use this detector to decide to do a Hot reload of a single path or not.
				// We need to filter out source maps and possibly some other to be able
				// to make that decision.
				irrelevantRe: regexp.MustCompile(`\.map$`),
			}

			changeDetector.PrepareNew()
			fs.Destination = hugofs.NewHashingFs(fs.Destination, changeDetector)
			c.changeDetector = changeDetector
		}

		if c.Cfg.GetBool("logPathWarnings") {
			fs.Destination = hugofs.NewCreateCountingFs(fs.Destination)
		}

		// To debug hard-to-find path issues.
		// fs.Destination = hugofs.NewStacktracerFs(fs.Destination, `fr/fr`)

		err = c.initFs(fs)
		if err != nil {
			close(c.created)
			return
		}

		var h *hugolib.HugoSites

		h, err = hugolib.NewHugoSites(*c.DepsCfg)
		c.hugoSites = h

		// 初始化完成
		close(c.created)
	})

	if err != nil {
		return err
	}

	cacheDir, err := helpers.GetCacheDir(sourceFs, config)
	if err != nil {
		return err
	}
	config.Set("cacheDir", cacheDir)

	cfg.Logger.Infoln("Using config file:", config.ConfigFileUsed())

	return nil
}

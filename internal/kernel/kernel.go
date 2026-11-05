// Package kernel 是 gsh 的微内核：插件装载、服务注册、事件总线。
// 一切能力都以插件形式挂载，内核只负责装载、服务注册与事件分发。
package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Plugin 是一切能力的载体。模型、工具、会话、UI 都以插件形式挂进内核。
type Plugin interface {
	// Name 插件名，全进程唯一。
	Name() string
	// Apply 装载点：在此注册服务、订阅事件。返回 cleanup（可为 nil），内核 Shutdown 时倒序调用。
	Apply(ctx *Ctx) error
}

// Ctx 传给每个插件的操作上下文。
type Ctx struct {
	k        *Kernel
	name     string // 所属插件名
	provided map[string]bool
}

// Name 返回当前插件名。
func (c *Ctx) Name() string { return c.name }

// Provide 注册一个服务。服务名即全进程唯一 key。
func (c *Ctx) Provide(name string, svc any) {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	if _, dup := c.k.services[name]; dup {
		panic(fmt.Sprintf("kernel: service %q provided twice (by %s)", name, c.name))
	}
	c.k.services[name] = svc
	c.provided[name] = true
}

// Config 返回 config.json 里 plugins.<本插件名> 的配置节，没有则返回 nil。
func (c *Ctx) Config() map[string]any {
	return c.k.pluginConfig(c.name)
}

// DataDir 返回本插件的数据目录（已确保存在）。会话文件等落盘内容放这里。
func (c *Ctx) DataDir() string { return c.k.dataDirFor(c.name) }

// BaseDir 返回程序目录：配置里的相对路径（workspace / skills）相对它解析。
// 未设置时退回当前工作目录，保持旧行为。
func (c *Ctx) BaseDir() string {
	if c.k.cfg != nil && c.k.cfg.BaseDir != "" {
		return c.k.cfg.BaseDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// DataDirFor 返回某插件的数据目录（宿主侧脚本、归档等需要定位数据的场景用）。
func (k *Kernel) DataDirFor(plugin string) string { return k.dataDirFor(plugin) }

func (k *Kernel) dataDirFor(plugin string) string {
	root := k.cfg.DataDir
	if root == "" {
		root = filepath.Join(os.TempDir(), "gsh-data")
	}
	dir := filepath.Join(root, plugin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(fmt.Sprintf("kernel: mkdir data dir for %s: %v", plugin, err))
	}
	return dir
}

// SetRuntime 发布一个运行时变量，供后续装载的插件读取。
// 用途：tools 插件把实际解析出的工作区路径、权限档位交给提示词等消费方，
// 避免多个插件各自按相对路径重算一遍而得到不同结果。
func (c *Ctx) SetRuntime(key string, v any) { c.k.SetValue(key, v) }

// Runtime 读取任意类型的运行时变量，缺失返回 nil。
func (c *Ctx) Runtime(key string) any { return c.k.Value(key) }

// RuntimeString 读取字符串型运行时变量，缺失或非字符串返回空串。
func (c *Ctx) RuntimeString(key string) string {
	s, _ := c.k.Value(key).(string)
	return s
}

// Emit 广播事件。订阅者 panic 不影响发布方（recover 隔离）。
func (c *Ctx) Emit(topic string, payload any) {
	c.k.emit(topic, payload)
}

// On 订阅事件。返回取消订阅函数。
func (c *Ctx) On(topic string, fn func(payload any)) func() {
	return c.k.on(topic, fn)
}

// OnShutdown 注册停机清理函数（倒序执行，先注册后跑）。
func (c *Ctx) OnShutdown(fn func()) {
	c.k.mu.Lock()
	c.k.disposers = append(c.k.disposers, struct {
		name string
		fn   func()
	}{c.name, fn})
	c.k.mu.Unlock()
}

// Use 按类型取一个已注册服务。未找到返回 error。
func Use[T any](k *Kernel, name string) (T, error) {
	var zero T
	k.mu.RLock()
	v, ok := k.services[name]
	k.mu.RUnlock()
	if !ok {
		return zero, fmt.Errorf("kernel: service %q not found", name)
	}
	t, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("kernel: service %q has unexpected type %T", name, v)
	}
	return t, nil
}

// Kernel 微内核本体。
type Kernel struct {
	mu       sync.RWMutex
	services map[string]any
	handlers map[string][]sub
	subSeq   uint64
	plugins  map[string]Plugin

	cfg       *Config
	cfgPath   string
	disposers []struct {
		name string
		fn   func()
	}
	loaded []string
	vars   map[string]any
}

// Config 进程级配置（从 config.json 读入）。
type Config struct {
	Plugins []string                  `json:"plugins"`     // 启用的插件名，按序装载
	Options map[string]map[string]any `json:"plugins_cfg"` // 每插件配置节
	DataDir string                    `json:"data_dir"`    // 数据根目录
	// Path 配置文件的绝对路径（不参与序列化）。设置面板据此写回。
	Path string `json:"-"`
	// BaseDir 程序目录（不参与序列化）。配置里的相对路径（workspace / skills）
	// 一律相对它解析，避免随启动时的当前工作目录漂移。
	BaseDir string `json:"-"`
}

func (k *Kernel) pluginConfig(name string) map[string]any {
	if k.cfg == nil || k.cfg.Options == nil {
		return nil
	}
	return k.cfg.Options[name]
}

// Options 返回某个插件的配置节（供 UI / 诊断读取，不要在热路径反复调用）。
func (k *Kernel) Options(name string) (map[string]any, bool) {
	if k.cfg == nil || k.cfg.Options == nil {
		return nil, false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.cfg.Options[name]
	return v, ok
}

// SetOptions 整体替换某插件的配置节（设置面板保存时用）。
func (k *Kernel) SetOptions(name string, opts map[string]any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cfg == nil {
		k.cfg = &Config{}
	}
	if k.cfg.Options == nil {
		k.cfg.Options = map[string]map[string]any{}
	}
	k.cfg.Options[name] = opts
}

// SetConfigPath 记录配置文件位置，供设置面板写回。
func (k *Kernel) SetConfigPath(p string) {
	k.mu.Lock()
	k.cfgPath = p
	k.mu.Unlock()
}

// ConfigPath 返回配置文件路径（可能为空：配置来自内存）。
func (k *Kernel) ConfigPath() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.cfgPath
}

// New 创建内核。
func New(cfg *Config) *Kernel {
	if cfg == nil {
		cfg = &Config{}
	}
	return &Kernel{
		services: map[string]any{},
		handlers: map[string][]sub{},
		plugins:  map[string]Plugin{},
		cfg:      cfg,
		cfgPath:  cfg.Path,
	}
}

// Load 装载一个插件并立刻执行其 Apply。
func (k *Kernel) Load(p Plugin) error {
	if _, dup := k.plugins[p.Name()]; dup {
		return fmt.Errorf("kernel: plugin %q loaded twice", p.Name())
	}
	c := &Ctx{k: k, name: p.Name(), provided: map[string]bool{}}
	if err := p.Apply(c); err != nil {
		return fmt.Errorf("kernel: plugin %s apply: %w", p.Name(), err)
	}
	k.mu.Lock()
	k.plugins[p.Name()] = p
	k.loaded = append(k.loaded, p.Name())
	k.mu.Unlock()
	return nil
}

// LoadedPlugins 返回已装载插件名（按装载序）。
func (k *Kernel) LoadedPlugins() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, len(k.loaded))
	copy(out, k.loaded)
	return out
}

// Shutdown 停机：事件清空，disposer 按装载倒序执行。
func (k *Kernel) Shutdown() {
	k.mu.Lock()
	k.handlers = map[string][]sub{}
	n := len(k.disposers)
	fns := make([]func(), n)
	for i := 0; i < n; i++ {
		fns[i] = k.disposers[n-1-i].fn
	}
	k.disposers = nil
	k.mu.Unlock()
	for _, fn := range fns {
		safeCall(fn)
	}
}

func (k *Kernel) emit(topic string, payload any) {
	k.mu.RLock()
	src := k.handlers[topic]
	hs := make([]sub, len(src))
	copy(hs, src)
	k.mu.RUnlock()
	for _, h := range hs {
		safe(func() { h.fn(payload) })
	}
}

// sub 带自增 ID 的订阅项，用于精确退订。
type sub struct {
	id uint64
	fn func(any)
}

func (k *Kernel) on(topic string, fn func(any)) func() {
	k.mu.Lock()
	k.subSeq++
	s := sub{id: k.subSeq, fn: fn}
	k.handlers[topic] = append(k.handlers[topic], s)
	k.mu.Unlock()
	return func() {
		k.mu.Lock()
		hs := k.handlers[topic]
		for i, h := range hs {
			if h.id == s.id {
				k.handlers[topic] = append(hs[:i:i], hs[i+1:]...)
				break
			}
		}
		k.mu.Unlock()
	}
}

func safeCall(fn func()) { defer func() { _ = recover() }(); fn() }
func safe(fn func())     { defer func() { _ = recover() }(); fn() }

// SortedPluginNames 返回全部插件名排序结果（调试用）。
func (k *Kernel) SortedPluginNames() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	names := make([]string, 0, len(k.plugins))
	for n := range k.plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Current 当前进程内核（单内核进程模型；loop 等运行时服务取用）。
var Current *Kernel

// Value 读运行时变量（如 activeSession）。
func (k *Kernel) Value(key string) any {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.vars[key]
}

// SetValue 写运行时变量。
func (k *Kernel) SetValue(key string, v any) {
	k.mu.Lock()
	if k.vars == nil {
		k.vars = map[string]any{}
	}
	k.vars[key] = v
	k.mu.Unlock()
}

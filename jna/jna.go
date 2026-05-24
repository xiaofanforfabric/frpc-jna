// Copyright 2026 Fan-ME-FRP-Launcher
//
// JNA 适配层 - 导出 C 接口供 Java 通过 JNA 调用
// 编译为 DLL: go build -buildmode=c-shared -o frpc_jna.dll
//
// Java JNA 接口示例:
// public interface FrpcJNA extends Library {
//     int FrpcStart(String configPath);
//     int FrpcStop();
//     int FrpcIsRunning();
//     String FrpcGetVersion();
//     void FrpcFreeString(String str);
//     String FrpcGetLastError();
//     void FrpcSetLogLevel(String level);
//     // 多实例接口
//     int FrpcStartWithId(int id, String configPath);
//     int FrpcStopWithId(int id);
//     int FrpcIsRunningWithId(int id);
//     int FrpcStopAll();
// }

package main

/*
#include <stdlib.h>
*/
import "C"
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/featuregate"
	"github.com/fatedier/frp/pkg/policy/security"
	"github.com/fatedier/frp/pkg/util/log"
)

// 服务实例管理 - 支持多实例
type serviceInstance struct {
	service *client.Service
	cancel  context.CancelFunc
	running bool
	done    chan struct{}
}

var (
	instances   map[int]*serviceInstance
	instancesMu sync.Mutex
	lastError   string
	lastErrorMu sync.Mutex
	nextID      int
)

func init() {
	instances = make(map[int]*serviceInstance)
}

func setLastError(err string) {
	lastErrorMu.Lock()
	lastError = err
	lastErrorMu.Unlock()
}

func getLastError() string {
	lastErrorMu.Lock()
	defer lastErrorMu.Unlock()
	return lastError
}

// 内部启动逻辑，返回 instanceID
func startInstance(cfgPath string) (int, error) {
	if cfgPath == "" {
		cfgPath = "frpc.ini"
	}

	// 初始化日志
	log.InitLogger("console", "info", 3, false)

	// 加载配置
	result, err := config.LoadClientConfigResult(cfgPath, true)
	if err != nil {
		return -1, fmt.Errorf("加载配置文件失败: %v", err)
	}
	if result.IsLegacyFormat {
		log.Warnf("[JNA] WARNING: ini format is deprecated, please use yaml/json/toml format instead!")
	}

	if len(result.Common.FeatureGates) > 0 {
		if err := featuregate.SetFromMap(result.Common.FeatureGates); err != nil {
			return -1, fmt.Errorf("设置 feature gates 失败: %v", err)
		}
	}

	// 创建配置源
	configSource := source.NewConfigSource()
	if err := configSource.ReplaceAll(result.Proxies, result.Visitors); err != nil {
		return -1, fmt.Errorf("设置配置源失败: %v", err)
	}

	var storeSource *source.StoreSource
	if result.Common.Store.IsEnabled() {
		storePath := result.Common.Store.Path
		if storePath != "" && cfgPath != "" && !filepath.IsAbs(storePath) {
			storePath = filepath.Join(filepath.Dir(cfgPath), storePath)
		}
		s, err := source.NewStoreSource(source.StoreSourceConfig{
			Path: storePath,
		})
		if err != nil {
			return -1, fmt.Errorf("创建 store source 失败: %v", err)
		}
		storeSource = s
	}

	aggregator := source.NewAggregator(configSource)
	if storeSource != nil {
		aggregator.SetStoreSource(storeSource)
	}

	proxyCfgs, visitorCfgs, err := aggregator.Load()
	if err != nil {
		return -1, fmt.Errorf("从配置源加载配置失败: %v", err)
	}

	proxyCfgs, visitorCfgs = config.FilterClientConfigurers(result.Common, proxyCfgs, visitorCfgs)
	proxyCfgs = config.CompleteProxyConfigurers(proxyCfgs)
	visitorCfgs = config.CompleteVisitorConfigurers(visitorCfgs)

	warning, err := validation.ValidateAllClientConfig(result.Common, proxyCfgs, visitorCfgs, security.NewUnsafeFeatures(nil))
	if warning != nil {
		log.Warnf("[JNA] 配置警告: %v", warning)
	}
	if err != nil {
		return -1, fmt.Errorf("配置验证失败: %v", err)
	}

	// 创建服务
	svr, err := client.NewService(client.ServiceOptions{
		Common:                 result.Common,
		ConfigSourceAggregator: aggregator,
		UnsafeFeatures:         security.NewUnsafeFeatures(nil),
		ConfigFilePath:         cfgPath,
	})
	if err != nil {
		return -1, fmt.Errorf("创建 frpc 服务失败: %v", err)
	}

	// 分配 ID
	instancesMu.Lock()
	id := nextID
	nextID++
	ctx, cancel := context.WithCancel(context.Background())
	inst := &serviceInstance{
		service: svr,
		cancel:  cancel,
		running: true,
		done:    make(chan struct{}),
	}
	instances[id] = inst
	instancesMu.Unlock()

	// 启动服务
	go func() {
		defer func() {
			instancesMu.Lock()
			inst.running = false
			instancesMu.Unlock()
			log.Infof("[JNA] frpc 服务 #%d 已停止", id)
		}()
		defer close(inst.done)

		log.Infof("[JNA] frpc 服务 #%d 已启动", id)
		if err := svr.Run(ctx); err != nil {
			errMsg := fmt.Sprintf("frpc 服务 #%d 运行出错: %v", id, err)
			setLastError(errMsg)
			log.Warnf("[JNA] %s", errMsg)
		}
	}()

	return id, nil
}

// 内部停止逻辑
func stopInstance(id int) error {
	instancesMu.Lock()
	inst, ok := instances[id]
	if !ok || !inst.running || inst.service == nil {
		instancesMu.Unlock()
		return nil
	}
	delete(instances, id)
	instancesMu.Unlock()

	// 步骤1: 先尝试优雅关闭
	// GracefulClose 内部调用 svr.cancel(nil)，
	// 触发 svr.Run() 退出 → svr.stop() → ctl.GracefulClose(500ms)
	// → ctl.pm.Close() → 每个 proxy 发送 CloseProxy 消息给服务端
	// → time.Sleep(500ms) 等待消息发送完成 → 关闭连接
	inst.service.GracefulClose(500 * 1000 * 1000) // 500ms in nanoseconds
	inst.cancel()

	// 步骤2: 直接关闭底层 TCP 连接，确保服务端立即检测到断开
	// 即使 CloseProxy 消息因网络延迟未发送完成，TCP FIN/RST 包
	// 也会让服务端立即知道客户端已离线
	if conn := inst.service.GetControlConn(); conn != nil {
		conn.Close()
		log.Infof("[JNA] frpc 服务 #%d 底层连接已强制关闭", id)
	}

	// 步骤3: 等待 svr.Run() 的 goroutine 完成
	// 最多等待 3 秒，避免死锁
	select {
	case <-inst.done:
	case <-time.After(3 * time.Second):
		log.Warnf("[JNA] frpc 服务 #%d 关闭超时", id)
	}

	log.Infof("[JNA] frpc 服务 #%d 已停止", id)
	return nil
}

//export FrpcStart
// 启动 frpc 客户端（兼容旧接口，单例模式）
// configPath: 配置文件路径 (UTF-8)
// 返回: 0=成功, -1=失败
func FrpcStart(configPath *C.char) C.int {
	cfgPath := C.GoString(configPath)
	id, err := startInstance(cfgPath)
	if err != nil {
		setLastError(err.Error())
		log.Warnf("[JNA] FrpcStart 失败: %s", err.Error())
		return -1
	}
	log.Infof("[JNA] FrpcStart 成功, instanceID=%d", id)
	return 0
}

//export FrpcStop
// 停止 frpc 客户端（兼容旧接口，停止所有实例）
// 返回: 0=成功
func FrpcStop() C.int {
	instancesMu.Lock()
	ids := make([]int, 0, len(instances))
	for id := range instances {
		ids = append(ids, id)
	}
	instancesMu.Unlock()

	for _, id := range ids {
		stopInstance(id)
	}
	log.Infof("[JNA] FrpcStop 已停止 %d 个实例", len(ids))
	return 0
}

//export FrpcIsRunning
// 检查是否有 frpc 实例在运行
// 返回: 1=有运行中的实例, 0=全部已停止
func FrpcIsRunning() C.int {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	for _, inst := range instances {
		if inst.running {
			return 1
		}
	}
	return 0
}

// ====== 多实例接口 ======

//export FrpcStartWithId
// 启动 frpc 客户端，返回实例 ID
// id: 由调用方指定的实例 ID（必须 >= 0）
// configPath: 配置文件路径 (UTF-8)
// 返回: 实际分配的 instanceID（>=0）, -1=失败
func FrpcStartWithId(id C.int, configPath *C.char) C.int {
	cfgPath := C.GoString(configPath)
	instanceID, err := startInstance(cfgPath)
	if err != nil {
		setLastError(err.Error())
		log.Warnf("[JNA] FrpcStartWithId 失败: %s", err.Error())
		return -1
	}
	log.Infof("[JNA] FrpcStartWithId 成功, instanceID=%d", instanceID)
	return C.int(instanceID)
}

//export FrpcStopWithId
// 停止指定 ID 的 frpc 实例
// id: 实例 ID
// 返回: 0=成功, -1=失败
func FrpcStopWithId(id C.int) C.int {
	err := stopInstance(int(id))
	if err != nil {
		setLastError(err.Error())
		return -1
	}
	return 0
}

//export FrpcIsRunningWithId
// 检查指定 ID 的 frpc 实例是否在运行
// id: 实例 ID
// 返回: 1=运行中, 0=已停止
func FrpcIsRunningWithId(id C.int) C.int {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	inst, ok := instances[int(id)]
	if ok && inst.running {
		return 1
	}
	return 0
}

//export FrpcStopAll
// 停止所有 frpc 实例
// 返回: 0=成功
func FrpcStopAll() C.int {
	instancesMu.Lock()
	ids := make([]int, 0, len(instances))
	for id := range instances {
		ids = append(ids, id)
	}
	instancesMu.Unlock()

	for _, id := range ids {
		stopInstance(id)
	}
	log.Infof("[JNA] FrpcStopAll 已停止 %d 个实例", len(ids))
	return 0
}

//export FrpcGetVersion
// 获取 frpc 版本号
// 返回: 版本号字符串 (需要调用 FrpcFreeString 释放)
func FrpcGetVersion() *C.char {
	return C.CString("0.69.0")
}

//export FrpcGetLastError
// 获取最后一次错误信息
// 返回: 错误字符串 (需要调用 FrpcFreeString 释放)
func FrpcGetLastError() *C.char {
	err := getLastError()
	if err == "" {
		return nil
	}
	return C.CString(err)
}

//export FrpcFreeString
// 释放由 DLL 分配的字符串
func FrpcFreeString(str *C.char) {
	if str != nil {
		C.free(unsafe.Pointer(str))
	}
}

//export FrpcSetLogLevel
// 设置日志级别
// level: "trace", "debug", "info", "warn", "error"
func FrpcSetLogLevel(level *C.char) {
	lvl := C.GoString(level)
	log.InitLogger("console", lvl, 3, false)
}

// 空 main 函数，编译 DLL 需要
func main() {}

// 确保 init 函数执行
func init() {
	// 设置 frp 环境变量
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	if os.Getenv("QUIC_GO_DISABLE_ECN") == "" {
		os.Setenv("QUIC_GO_DISABLE_ECN", "true")
	}
	fmt.Println("frpc_jna.dll loaded")
}

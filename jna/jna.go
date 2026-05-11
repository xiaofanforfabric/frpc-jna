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
	"unsafe"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/policy/featuregate"
	"github.com/fatedier/frp/pkg/policy/security"
	"github.com/fatedier/frp/pkg/util/log"
)

// 服务实例管理
var (
	activeService  *client.Service
	serviceCancel  context.CancelFunc
	serviceMu      sync.Mutex
	serviceRunning bool
	lastError      string
	lastErrorMu    sync.Mutex
)

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

//export FrpcStart
// 启动 frpc 客户端
// configPath: 配置文件路径 (UTF-8)
// 返回: 0=成功, -1=失败
func FrpcStart(configPath *C.char) C.int {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	if serviceRunning {
		return 0 // 已经在运行
	}

	cfgPath := C.GoString(configPath)
	if cfgPath == "" {
		cfgPath = "frpc.ini"
	}

	// 初始化日志
	log.InitLogger("console", "info", 3, false)

	// 加载配置
	result, err := config.LoadClientConfigResult(cfgPath, true)
	if err != nil {
		errMsg := "加载配置文件失败: " + err.Error()
		setLastError(errMsg)
		log.Warnf("[JNA] %s", errMsg)
		return -1
	}
	if result.IsLegacyFormat {
		log.Warnf("[JNA] WARNING: ini format is deprecated, please use yaml/json/toml format instead!")
	}

	if len(result.Common.FeatureGates) > 0 {
		if err := featuregate.SetFromMap(result.Common.FeatureGates); err != nil {
			errMsg := "设置 feature gates 失败: " + err.Error()
			setLastError(errMsg)
			log.Warnf("[JNA] %s", errMsg)
			return -1
		}
	}

	// 创建配置源
	configSource := source.NewConfigSource()
	if err := configSource.ReplaceAll(result.Proxies, result.Visitors); err != nil {
		errMsg := "设置配置源失败: " + err.Error()
		setLastError(errMsg)
		log.Warnf("[JNA] %s", errMsg)
		return -1
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
			errMsg := "创建 store source 失败: " + err.Error()
			setLastError(errMsg)
			log.Warnf("[JNA] %s", errMsg)
			return -1
		}
		storeSource = s
	}

	aggregator := source.NewAggregator(configSource)
	if storeSource != nil {
		aggregator.SetStoreSource(storeSource)
	}

	proxyCfgs, visitorCfgs, err := aggregator.Load()
	if err != nil {
		errMsg := "从配置源加载配置失败: " + err.Error()
		setLastError(errMsg)
		log.Warnf("[JNA] %s", errMsg)
		return -1
	}

	proxyCfgs, visitorCfgs = config.FilterClientConfigurers(result.Common, proxyCfgs, visitorCfgs)
	proxyCfgs = config.CompleteProxyConfigurers(proxyCfgs)
	visitorCfgs = config.CompleteVisitorConfigurers(visitorCfgs)

	warning, err := validation.ValidateAllClientConfig(result.Common, proxyCfgs, visitorCfgs, security.NewUnsafeFeatures(nil))
	if warning != nil {
		log.Warnf("[JNA] 配置警告: %v", warning)
	}
	if err != nil {
		errMsg := "配置验证失败: " + err.Error()
		setLastError(errMsg)
		log.Warnf("[JNA] %s", errMsg)
		return -1
	}

	// 创建服务
	svr, err := client.NewService(client.ServiceOptions{
		Common:                 result.Common,
		ConfigSourceAggregator: aggregator,
		UnsafeFeatures:         security.NewUnsafeFeatures(nil),
		ConfigFilePath:         cfgPath,
	})
	if err != nil {
		errMsg := "创建 frpc 服务失败: " + err.Error()
		setLastError(errMsg)
		log.Warnf("[JNA] %s", errMsg)
		return -1
	}

	// 启动服务
	ctx, cancel := context.WithCancel(context.Background())
	serviceCancel = cancel
	activeService = svr
	serviceRunning = true

	go func() {
		defer func() {
			serviceMu.Lock()
			serviceRunning = false
			activeService = nil
			serviceMu.Unlock()
			log.Infof("[JNA] frpc 服务已停止")
		}()

		log.Infof("[JNA] frpc 服务已启动")
		if err := svr.Run(ctx); err != nil {
			errMsg := "frpc 服务运行出错: " + err.Error()
			setLastError(errMsg)
			log.Warnf("[JNA] %s", errMsg)
		}
	}()

	return 0
}

//export FrpcStop
// 停止 frpc 客户端
// 返回: 0=成功, -1=失败
func FrpcStop() C.int {
	serviceMu.Lock()
	defer serviceMu.Unlock()

	if !serviceRunning || activeService == nil {
		return 0
	}

	activeService.GracefulClose(500 * 1000 * 1000) // 500ms in nanoseconds
	serviceCancel()
	serviceRunning = false
	activeService = nil

	log.Infof("[JNA] frpc 服务已停止")
	return 0
}

//export FrpcIsRunning
// 检查 frpc 是否在运行
// 返回: 1=运行中, 0=已停止
func FrpcIsRunning() C.int {
	serviceMu.Lock()
	defer serviceMu.Unlock()
	if serviceRunning {
		return 1
	}
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

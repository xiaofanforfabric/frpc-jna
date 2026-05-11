# frpc-jna

> 基于 [fatedier/frp](https://github.com/fatedier/frp) v0.69.0 的上游分支，添加了 **JNA (Java Native Access)** 支持，让 Java 程序可以直接调用 frpc 的全部能力，无需自己实现 frp 协议。

## 本分支与上游的关系

本仓库是 frp 的一个**上游跟踪分支**，在保持与原版 frp 完全兼容的基础上，额外提供了：

- **`jna/`** — JNA 适配层，将 frpc 的核心功能导出为 C 接口 DLL
- **`bin/frpc_jna.dll`** — 预编译的 JNA DLL，Java 通过 JNA 直接加载调用

其余 `client/`、`pkg/`、`cmd/` 等目录与上游 [fatedier/frp](https://github.com/fatedier/frp) 保持同步，可通过 Renovate/Dependabot 自动跟踪上游更新。

## 为什么需要 JNA 版本？

传统的 frp Java 集成方案需要自己实现 frp 的 V1/V2 协议（登录、心跳、代理管理等），工作量大且容易出错。通过 JNA 直接调用 Go 编译的 DLL：

- ✅ **零协议实现** — Java 端无需关心 frp 内部协议
- ✅ **完整功能** — 支持所有 frp 特性（TCP/HTTP/HTTPS/QUIC/STCP 等）
- ✅ **同步上游** — frp 新功能自动获得，无需手动移植
- ✅ **性能无损** — Go 原生编译，无中间层开销

## 编译 DLL

### 前置条件

- Go 1.21+
- GCC (MinGW-w64，Windows 编译需要)
- 设置环境变量：`set CC=gcc`（覆盖默认的 zig 编译器）

### 编译命令

```bash
cd implementation/frp

# 编译 frpc 和 frps（可选）
go build -buildvcs=false -tags "frpc,noweb" -o bin/frpc.exe ./cmd/frpc/
go build -buildvcs=false -tags "frps,noweb" -o bin/frps.exe ./cmd/frps/

# 编译 JNA DLL（核心产物）
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=1
set CC=gcc
go build -buildvcs=false -buildmode=c-shared -trimpath -ldflags "-s -w" -tags "frpc,noweb" -o bin/frpc_jna.dll ./jna/
```

编译产物位于 `bin/` 目录：

| 文件 | 说明 |
|------|------|
| `frpc.exe` | FRP 客户端 |
| `frps.exe` | FRP 服务端 |
| `frpc_jna.dll` | **JNA 适配 DLL**（Java 调用入口） |
| `frpc_jna.h` | C 头文件（自动生成，仅供参考） |

## Java 集成

### 1. 添加依赖

```groovy
// build.gradle
dependencies {
    implementation 'net.java.dev.jna:jna:5.14.0'
}
```

### 2. 加载 DLL

将 `frpc_jna.dll` 放在项目根目录或 `java.library.path` 中。

### 3. 调用示例

```java
import com.sun.jna.Library;
import com.sun.jna.Native;

public interface FrpcJNA extends Library {
    int FrpcStart(String configPath);
    int FrpcStop();
    int FrpcIsRunning();
    String FrpcGetVersion();
    String FrpcGetLastError();
    void FrpcFreeString(String str);
    void FrpcSetLogLevel(String level);
}

// 使用
FrpcJNA frpc = Native.load("frpc_jna", FrpcJNA.class);
frpc.FrpcStart("frpc.ini");
// ... 运行中 ...
frpc.FrpcStop();
```

完整 Java 封装类见 [FrpcJnaBridge.java](../../src/main/java/com/xiaofan/launcher/frpc/FrpcJnaBridge.java)。

## 导出的 C 接口

| 函数 | 说明 |
|------|------|
| `int FrpcStart(char* configPath)` | 启动 frpc 客户端，传入配置文件路径 |
| `int FrpcStop()` | 停止 frpc 客户端 |
| `int FrpcIsRunning()` | 检查运行状态（1=运行中，0=已停止） |
| `char* FrpcGetVersion()` | 获取版本号（需调用 FrpcFreeString 释放） |
| `char* FrpcGetLastError()` | 获取最后一次错误信息（需调用 FrpcFreeString 释放） |
| `void FrpcFreeString(char* str)` | 释放 DLL 分配的字符串 |
| `void FrpcSetLogLevel(char* level)` | 设置日志级别（trace/debug/info/warn/error） |

## 扩展开发

### 不修改源码的扩展方式

frp 官方提供了多种扩展点，无需修改 `client/`、`pkg/` 等核心代码：

1. **Server Plugin** — 外部 HTTP RPC 服务，在 Login/NewProxy 等操作时回调
2. **ConnectorCreator** — 自定义连接器，替换 frpc 连接 frps 的方式
3. **HandleWorkConnCb** — 工作连接回调，处理新的工作连接

以上扩展点均在 `jna/jna.go` 中通过 `ServiceOptions` 传入即可。

### 新增 JNA 导出函数

如需新增 DLL 导出函数，编辑 `jna/jna.go`，按以下模板添加：

```go
//export YourNewFunction
func YourNewFunction(param *C.char) C.int {
    // 你的逻辑
    return 0
}
```

然后重新编译 DLL 即可，Java 端同步更新接口定义。

## 上游同步

本分支通过 Renovate 依赖机器人自动跟踪上游 frp 更新：

1. 上游发布新版本 → Renovate 自动创建 PR
2. 检查 `jna/jna.go` 与新版本兼容性
3. 合并 PR，GitHub Actions 自动编译发布新 DLL

## 许可证

本项目基于 frp 的 Apache License 2.0 许可证，详见 [LICENSE](LICENSE)。

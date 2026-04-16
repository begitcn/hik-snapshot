# 海康威视抓图服务

基于海康威视 HCNetSDK 的 HTTP 抓图微服务，支持多设备并发抓图，内置登录会话缓存机制。

## 目录结构

```
hik-snapshot-go/
├── main.go                 # 主程序入口
├── Dockerfile              # Docker 构建文件
├── scripts/
│   └── preprocess_header.py    # HCNetSDK.h 头文件预处理脚本
├── sdk/
│   ├── include/            # 海康 SDK 头文件
│   │   └── HCNetSDK.h
│   └── libs/               # 海康 SDK 动态库
│       ├── libhcnetsdk.so
│       └── HCNetSDKCom/    # 依赖组件库
└── README.md
```

## 技术架构

### 整体架构

```
┌─────────────────────────────────────────────────────┐
│                    HTTP Server (:9876)              │
│              Read/Write Timeout: 10s                │
├─────────────────────────────────────────────────────┤
│  /snapshot  │  /health                              │
│  抓图接口    │  健康检查                              │
├─────────────┴───────────────────────────────────────┤
│              Session Cache (sync.Map)               │
│         缓存已登录设备的 loginID                      │
│         TTL: 30分钟，自动清理过期会话                  │
├─────────────────────────────────────────────────────┤
│              Login Lock (sync.Map)                  │
│         设备级登录锁，防止并发重复登录                 │
├─────────────────────────────────────────────────────┤
│              HCNetSDK (CGO 调用)                    │
│         NET_DVR_Login_V30                           │
│         NET_DVR_CaptureJPEGPicture_NEW             │
├─────────────────────────────────────────────────────┤
│              海康威视摄像机                           │
└─────────────────────────────────────────────────────┘
```

### 核心组件

| 组件 | 说明 |
|------|------|
| HTTP Server | 监听 9876 端口，读写超时 10 秒 |
| Session Cache | 基于 `sync.Map` 的并发安全登录缓存，TTL 30 分钟 |
| Login Lock | 设备级互斥锁，防止并发登录同一设备 |
| Session Cleanup | 后台协程每 5 分钟清理过期会话 |
| Graceful Shutdown | 优雅关闭，释放所有 SDK 资源 |

## 代码说明

### main.go 主程序

#### 1. SDK 初始化

```go
func main() {
    // 初始化 SDK（必须首先调用）
    if C.NET_DVR_Init() == 0 {
        log.Fatal("SDK Init Failed")
    }

    // 设置连接超时：3秒超时，重试3次
    C.NET_DVR_SetConnectTime(C.DWORD(3000), C.DWORD(3))

    // 设置断线重连：30秒间隔，启用重连
    C.NET_DVR_SetReconnect(C.DWORD(30000), 1)
}
```

#### 2. 登录缓存机制（双重检查锁）

```go
type SessionInfo struct {
    LoginID    int
    LastActive time.Time
}

func getOrCreateSession(key, ip string, port int, user, pass string) (int, error) {
    // 1. 快速路径：检查缓存
    if info, ok := sessionMap.Load(key); ok {
        return info.(*SessionInfo).LoginID, nil
    }

    // 2. 获取设备级锁，防止并发登录
    lock := getLoginLock(key)
    lock.Lock()
    defer lock.Unlock()

    // 3. 双重检查：可能其他协程已登录成功
    if info, ok := sessionMap.Load(key); ok {
        return info.(*SessionInfo).LoginID, nil
    }

    // 4. 执行登录
    loginID := int(C.NET_DVR_Login_V30(...))

    // 5. 缓存登录信息
    sessionMap.Store(key, &SessionInfo{
        LoginID:    loginID,
        LastActive: time.Now(),
    })

    return loginID, nil
}
```

**并发登录保护流程：**

```
请求1 ──┬── 检查缓存(未命中) ── 获取锁 ── 执行登录 ── 缓存结果 ──┐
        │                                                      │
请求2 ──┼── 检查缓存(未命中) ── 等待锁 ── 获取锁 ── 双重检查 ──┤
        │                                        (命中缓存)     │
        │                                                      │
请求3 ──┴── 检查缓存(命中) ── 直接返回 ─────────────────────────┘
```

#### 3. 会话过期清理

```go
func sessionCleanup() {
    ticker := time.NewTicker(5 * time.Minute)
    for range ticker.C {
        now := time.Now()
        sessionMap.Range(func(key, value interface{}) bool {
            info := value.(*SessionInfo)
            if now.Sub(info.LastActive) > sessionTTL {
                C.NET_DVR_Logout_V30(C.LONG(info.LoginID))
                sessionMap.Delete(key)
                log.Printf("[缓存清理] 移除过期会话: %s", key)
            }
            return true
        })
    }
}
```

#### 4. 优雅关闭

```go
go func() {
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    sig := <-sigChan

    // 1. 登出所有设备
    sessionMap.Range(func(key, value interface{}) bool {
        info := value.(*SessionInfo)
        C.NET_DVR_Logout_V30(C.LONG(info.LoginID))
        return true
    })

    // 2. 清理 SDK
    C.NET_DVR_Cleanup()

    // 3. 关闭 HTTP 服务
    server.Shutdown(ctx)
}()
```

#### 5. 抓图失败自动恢复

```go
imgData, err := capturePicture(loginID, channel)
if err != nil {
    // 抓图失败，清除缓存会话，下次请求重新登录
    sessionMap.Delete(key)
    C.NET_DVR_Logout_V30(C.LONG(loginID))
    log.Printf("[抓图失败] 已清除缓存，下次将重新登录")
    return err
}

// 更新最后活动时间
if info, ok := sessionMap.Load(key); ok {
    info.(*SessionInfo).LastActive = time.Now()
}
```

### 头文件预处理 (preprocess_header.py)

由于海康 SDK 的头文件包含 C++ 语法，需要预处理以兼容 C 语言：

| 处理项 | 原始 C++ | 处理后 C |
|--------|----------|----------|
| extern "C" | `extern "C"` | 移除 |
| 调用约定 | `__stdcall` | 移除 |
| 回调宏 | `CALLBACK` | 移除 |
| 默认参数 | `= 3000` | 移除 |
| 十六进制 enum | `x01` | `= 0x01` |

## 接口说明

### 1. 抓图接口

**请求：**
```
GET /snapshot?ip=<设备IP>&port=<端口>&user=<用户名>&pass=<密码>&channel=<通道号>
```

**参数说明：**

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| ip | string | 是 | - | 设备 IP 地址 |
| port | int | 否 | 8000 | 端口号 |
| user | string | 是 | - | 登录用户名 |
| pass | string | 是 | - | 登录密码 |
| channel | int | 否 | 1 | 通道号（NVR 需指定） |

**响应：**

| 状态码 | 说明 | Content-Type |
|--------|------|--------------|
| 200 | 成功，返回 JPEG 图片 | image/jpeg |
| 400 | 参数不全 | application/json |
| 401 | 登录失败（用户名或密码错误） | application/json |
| 500 | 抓图失败（SDK 错误） | application/json |

**响应头：**

| Header | 说明 |
|--------|------|
| X-Response-Time | 请求处理耗时 |

**示例：**
```bash
# 基础抓图
curl "http://localhost:9876/snapshot?ip=192.168.1.64&user=admin&pass=Admin123" -o snapshot.jpg

# 指定端口和通道
curl "http://localhost:9876/snapshot?ip=192.168.1.64&port=8080&user=admin&pass=Admin123&channel=2" -o snapshot.jpg

# 错误响应示例
curl "http://localhost:9876/snapshot?ip=192.168.1.64&user=admin&pass=wrong"
# 返回: {"error":"登录失败","detail":"SDK错误码: 1"}
```

### 2. 健康检查接口

**请求：**
```
GET /health
```

**响应：**
```json
{
  "status": "ok"
}
```

## 性能特性

### 并发处理

| 特性 | 实现方式 |
|------|----------|
| 并发安全 | `sync.Map` 存储会话缓存 |
| 登录防抖 | 设备级互斥锁，防止并发重复登录 |
| 超时控制 | HTTP 读写超时 10 秒 |
| 资源复用 | 登录会话复用，跳过重复登录 |

### 高可用

| 特性 | 实现方式 |
|------|----------|
| 会话过期 | 30 分钟无活动自动清理 |
| 故障恢复 | 抓图失败自动清除会话，下次重新登录 |
| 断线重连 | SDK 层自动重连（30 秒间隔） |
| 优雅关闭 | SIGTERM/SIGINT 信号处理，释放所有资源 |

### 性能指标

| 场景 | 耗时 |
|------|------|
| 缓存命中抓图 | ~200-500ms |
| 首次登录抓图 | ~1-2s |
| 并发 100 请求 | 无阻塞 |

## 部署说明

### Docker 部署

```bash
# 构建镜像
docker build -t hik-snapshot-service:v1 .

# 运行容器
docker run -d \
  --name hik-snapshot \
  -p 9876:9876 \
  --restart unless-stopped \
  hik-snapshot-service:v1
```

### 镜像导出与导入

适用于离线环境部署，将镜像打包后在目标机器导入。

#### 导出镜像

```bash
# 导出为 tar 文件
docker save hik-snapshot-service:v1 -o hik-snapshot-service-v1.tar

# 压缩导出（推荐，文件更小）
docker save hik-snapshot-service:v1 | gzip > hik-snapshot-service-v1.tar.gz

# 查看导出文件大小
ls -lh hik-snapshot-service-v1.tar*
```

#### 导入镜像

```bash
# 从 tar 文件导入
docker load -i hik-snapshot-service-v1.tar

# 从压缩文件导入
docker load -i hik-snapshot-service-v1.tar.gz

# 或者使用 gunzip 解压后导入
gunzip -c hik-snapshot-service-v1.tar.gz | docker load

# 验证导入成功
docker images | grep hik-snapshot-service
```

#### 离线部署示例

```bash
# === 在有网络的机器上 ===
# 1. 构建镜像
docker build -t hik-snapshot-service:v1 .

# 2. 导出镜像
docker save hik-snapshot-service:v1 | gzip > hik-snapshot-service-v1.tar.gz

# 3. 传输文件到目标机器（示例）
scp hik-snapshot-service-v1.tar.gz user@target-server:/opt/images/

# === 在目标机器上 ===
# 4. 导入镜像
docker load -i /opt/images/hik-snapshot-service-v1.tar.gz

# 5. 运行容器
docker run -d \
  --name hik-snapshot \
  -p 9876:9876 \
  --restart unless-stopped \
  hik-snapshot-service:v1
```

### Docker Compose 部署

```yaml
version: '3.8'
services:
  hik-snapshot:
    build: .
    image: hik-snapshot-service:v1
    container_name: hik-snapshot
    ports:
      - "9876:9876"
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9876/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 10s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

```bash
# 启动服务
docker-compose up -d

# 查看日志
docker-compose logs -f hik-snapshot

# 查看状态
docker-compose ps
```

## 环境要求

### 编译环境

- Go 1.20+
- GCC (支持 CGO)
- Python 3（头文件预处理）

### 运行环境

- Docker 或 Linux 系统
- 依赖库：libxml2, libxcb1, libx11-6

### SDK 版本

- 海康威视 HCNetSDK Linux 版本
- 支持设备：海康威视网络摄像机、NVR 等

## 常见问题

### 1. 编译报错：HCNetSDK.h 语法错误

**原因：** 头文件包含 C++ 语法

**解决：** 确保预处理脚本正确执行，检查 `scripts/preprocess_header.py`

### 2. 运行报错：找不到动态库

**原因：** LD_LIBRARY_PATH 未正确设置

**解决：** Docker 中已自动设置，手动运行需：
```bash
export LD_LIBRARY_PATH=/app/libs:/app/libs/HCNetSDKCom:$LD_LIBRARY_PATH
```

### 3. 抓图失败，错误码 23

**原因：** 设备通道号错误

**解决：** 使用 `channel` 参数指定正确的通道号
- 普通摄像机：channel=1
- NVR：根据实际接入通道号设置（如 channel=1, 2, 3...）

### 4. 请求超时

**原因：** 设备响应慢或网络问题

**解决：**
- 检查设备网络连通性
- 服务默认超时 10 秒，可根据需要调整

### 5. 并发请求性能下降

**原因：** 大量首次登录请求

**解决：** 预热缓存，服务启动后先请求一次各设备

## 更新日志

### v1.1.0
- 新增：设备级登录锁，防止并发重复登录
- 新增：会话过期自动清理（TTL 30 分钟）
- 新增：抓图失败自动恢复机制
- 新增：优雅关闭，正确释放 SDK 资源
- 新增：请求超时控制（10 秒）
- 新增：请求耗时日志
- 新增：channel 参数支持多通道
- 新增：port 参数支持自定义端口
- 修复：缓存 key 包含密码，防止密码错误也能抓图

### v1.0.0
- 初始版本
- 支持基础抓图功能
- 登录会话缓存
- 健康检查接口
- Docker 部署支持

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
├─────────────────────────────────────────────────────┤
│  /snapshot  │  /health                              │
│  抓图接口    │  健康检查                              │
├─────────────┴───────────────────────────────────────┤
│              Session Cache (sync.Map)               │
│         缓存已登录设备的 loginID                      │
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
| HTTP Server | 监听 9876 端口，提供 RESTful 接口 |
| Session Cache | 基于 `sync.Map` 的并发安全登录缓存 |
| HCNetSDK | 海康威视官方 SDK，通过 CGO 调用 |

## 代码说明

### main.go 主程序

#### 1. SDK 初始化

```go
func main() {
    // 初始化 SDK（必须首先调用）
    if C.NET_DVR_Init() == 0 {
        log.Fatal("SDK Init Failed")
    }
    defer C.NET_DVR_Cleanup()

    // 设置连接超时：2秒超时，重试1次
    C.NET_DVR_SetConnectTime(C.DWORD(2000), C.DWORD(1))
}
```

#### 2. 登录缓存机制

```go
// 缓存已登录的设备 ID（并发安全）
var sessionMap sync.Map

func getLoginID(ip, port, user, pass string) int {
    // 缓存 key 格式: ip-user-pass
    key := fmt.Sprintf("%s-%s-%s", ip, user, pass)

    // 命中缓存，直接返回 loginID
    if id, ok := sessionMap.Load(key); ok {
        return id.(int)
    }

    // 未命中，执行登录
    loginID := C.NET_DVR_Login_V30(cIP, 8000, cUser, cPass, &deviceInfo)

    // 登录成功后缓存
    if loginID >= 0 {
        sessionMap.Store(key, int(loginID))
    }
    return int(loginID)
}
```

**缓存策略说明：**

| 场景 | 行为 |
|------|------|
| 首次请求 | 执行登录 → 缓存 loginID → 抓图 |
| 相同凭证再次请求 | 命中缓存 → 直接抓图（跳过登录） |
| 不同密码请求 | 未命中缓存 → 执行登录 → 登录失败 |

#### 3. 抓图流程

```go
func snapshotHandler(w http.ResponseWriter, r *http.Request) {
    // 1. 获取登录 session
    loginID := getLoginID(ip, port, user, pass)

    // 2. 设置 JPEG 参数
    jpegPara := C.NET_DVR_JPEGPARA{
        wPicQuality: 0,    // 画质：0=最好
        wPicSize:    0xff, // 分辨率：自动
    }

    // 3. 分配缓冲区（2MB）
    buffer := C.malloc(C.size_t(2 * 1024 * 1024))

    // 4. 调用 SDK 抓图
    res := C.NET_DVR_CaptureJPEGPicture_NEW(loginID, channel, &jpegPara, ...)

    // 5. 返回 JPEG 数据
    w.Header().Set("Content-Type", "image/jpeg")
    w.Write(data)
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
GET /snapshot?ip=<设备IP>&port=<端口>&user=<用户名>&pass=<密码>
```

**参数说明：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| ip | string | 是 | 设备 IP 地址 |
| port | string | 否 | 端口号（默认 8000） |
| user | string | 是 | 登录用户名 |
| pass | string | 是 | 登录密码 |

**响应：**

| 状态码 | 说明 | Content-Type |
|--------|------|--------------|
| 200 | 成功，返回 JPEG 图片 | image/jpeg |
| 400 | 参数不全 | text/plain |
| 401 | 登录失败（用户名或密码错误） | text/plain |
| 500 | 抓图失败（SDK 错误） | text/plain |

**示例：**
```bash
# 抓图并保存
curl "http://localhost:9876/snapshot?ip=192.168.1.64&user=admin&pass=Admin123" -o snapshot.jpg

# 错误密码
curl "http://localhost:9876/snapshot?ip=192.168.1.64&user=admin&pass=wrong"
# 返回: 登录失败 (401)
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

## 部署说明

### Docker 部署

```bash
# 构建镜像
docker build -t hik-snapshot-service:v1 .

# 运行容器
docker run -d \
  --name hik-snapshot \
  -p 9876:9876 \
  hik-snapshot-service:v1
```

### Docker Compose 部署

```yaml
version: '3.8'
services:
  hik-snapshot:
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
```

```bash
# 启动服务
docker-compose up -d

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

**解决：** 大多数设备通道号为 1，NVR 需根据实际通道调整

### 4. 缓存导致密码变更后仍能抓图

**说明：** 这是一个特性设计，避免频繁登录

**注意：** 如需清除缓存，重启服务即可

## 更新日志

### v1.0.0
- 初始版本
- 支持基础抓图功能
- 登录会话缓存
- 健康检查接口
- Docker 部署支持

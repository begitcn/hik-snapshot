# --- 第一阶段：编译 ---
FROM docker.m.daocloud.io/library/golang:1.20-bullseye AS builder

WORKDIR /app

# 复制所有文件
COPY . .

# 【核心步骤：使用 Python 脚本预处理海康头文件】
# 修复 C++ 语法使其兼容 C 语言（extern "C", __stdcall, 默认参数, enum 十六进制值等）
RUN python3 /app/scripts/preprocess_header.py /app/sdk/include/HCNetSDK.h

# 编译 Go 程序
RUN CGO_ENABLED=1 GOOS=linux go build -o hik-service main.go

# --- 第二阶段：运行 ---
FROM docker.m.daocloud.io/library/debian:bullseye-slim

WORKDIR /app

# 换源并安装底层依赖
RUN sed -i 's/deb.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list && \
    sed -i 's/security.debian.org/mirrors.aliyun.com/g' /etc/apt/sources.list && \
    apt-get update && apt-get install -y \
    libxml2 \
    libxcb1 \
    libx11-6 \
    libc6 \
    && rm -rf /var/lib/apt/lists/*

# 拷贝成品
COPY --from=builder /app/hik-service .
COPY --from=builder /app/sdk/libs /app/libs

# 设置环境变量
ENV LD_LIBRARY_PATH=/app/libs:/app/libs/HCNetSDKCom

EXPOSE 9876

CMD ["./hik-service"]
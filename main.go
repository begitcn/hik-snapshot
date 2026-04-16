package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

/*
#cgo LDFLAGS: -L./sdk/libs -lhcnetsdk
#cgo CFLAGS: -I./sdk/include

#include "HCNetSDK.h"
#include <stdlib.h>
*/
import "C"

// SessionInfo 缓存的登录会话信息
type SessionInfo struct {
	LoginID    int
	LastActive time.Time
}

// 缓存已登录的设备 ID
var (
	sessionMap     sync.Map
	loginLocks     sync.Map // 登录锁，防止并发登录同一设备
	sessionTTL     = 30 * time.Minute
	cleanupTick    = 5 * time.Minute
	requestTimeout = 10 * time.Second
)

func main() {
	// 1. 初始化 SDK
	if C.NET_DVR_Init() == 0 {
		log.Fatal("SDK Init Failed")
	}
	log.Println("SDK 初始化成功")

	// 设置连接超时和重连
	C.NET_DVR_SetConnectTime(C.DWORD(3000), C.DWORD(3))
	C.NET_DVR_SetReconnect(C.DWORD(30000), 1)
	log.Println("SDK 连接参数配置完成")

	// 2. 启动缓存清理协程
	go sessionCleanup()

	// 3. HTTP 路由
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", snapshotHandler)
	mux.HandleFunc("/health", healthHandler)

	server := &http.Server{
		Addr:         ":9876",
		Handler:      mux,
		ReadTimeout:  requestTimeout,
		WriteTimeout: requestTimeout,
	}

	// 4. 优雅关闭
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan
		log.Printf("收到信号 %v，正在关闭服务...", sig)

		// 清理所有登录会话
		sessionMap.Range(func(key, value interface{}) bool {
			info := value.(*SessionInfo)
			C.NET_DVR_Logout_V30(C.LONG(info.LoginID))
			log.Printf("登出设备: %s", key)
			return true
		})

		// 清理 SDK
		C.NET_DVR_Cleanup()
		log.Println("SDK 资源释放完成")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Println("海康抓图服务启动在 :9876")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务启动失败: %v", err)
	}
	log.Println("服务已关闭")
}

// sessionCleanup 定期清理过期会话
func sessionCleanup() {
	ticker := time.NewTicker(cleanupTick)
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

// getLoginLock 获取设备登录锁，防止并发登录
func getLoginLock(key string) *sync.Mutex {
	lockI, _ := loginLocks.LoadOrStore(key, &sync.Mutex{})
	return lockI.(*sync.Mutex)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func snapshotHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ip := r.URL.Query().Get("ip")
	portStr := r.URL.Query().Get("port")
	user := r.URL.Query().Get("user")
	pass := r.URL.Query().Get("pass")
	channelStr := r.URL.Query().Get("channel")

	if ip == "" || user == "" || pass == "" {
		http.Error(w, `{"error":"参数不全","required":"ip,user,pass"}`, http.StatusBadRequest)
		return
	}

	// 解析端口，默认 8000
	port := 8000
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			port = p
		}
	}

	// 解析通道号，默认 1
	channel := 1
	if channelStr != "" {
		if c, err := strconv.Atoi(channelStr); err == nil && c > 0 {
			channel = c
		}
	}

	key := fmt.Sprintf("%s:%d-%s-%s", ip, port, user, pass)

	loginID, err := getOrCreateSession(key, ip, port, user, pass)
	if err != nil {
		log.Printf("[登录失败] %s - %v", key, err)
		http.Error(w, fmt.Sprintf(`{"error":"登录失败","detail":"%v"}`, err), http.StatusUnauthorized)
		return
	}

	// 执行抓图
	imgData, err := capturePicture(loginID, channel)
	if err != nil {
		// 抓图失败，清除缓存会话，下次重新登录
		sessionMap.Delete(key)
		C.NET_DVR_Logout_V30(C.LONG(loginID))
		log.Printf("[抓图失败] %s channel=%d - %v，已清除缓存", key, channel, err)
		http.Error(w, fmt.Sprintf(`{"error":"抓图失败","detail":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	// 更新最后活动时间
	if info, ok := sessionMap.Load(key); ok {
		info.(*SessionInfo).LastActive = time.Now()
	}

	log.Printf("[抓图成功] %s channel=%d size=%dKB 耗时=%v",
		key, channel, len(imgData)/1024, time.Since(start))

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Response-Time", time.Since(start).String())
	w.Write(imgData)
}

// getOrCreateSession 获取或创建登录会话
func getOrCreateSession(key, ip string, port int, user, pass string) (int, error) {
	// 先尝试从缓存获取
	if info, ok := sessionMap.Load(key); ok {
		return info.(*SessionInfo).LoginID, nil
	}

	// 获取设备登录锁，防止并发登录同一设备
	lock := getLoginLock(key)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查，可能其他协程已经登录成功
	if info, ok := sessionMap.Load(key); ok {
		return info.(*SessionInfo).LoginID, nil
	}

	// 执行登录
	cIP := C.CString(ip)
	cUser := C.CString(user)
	cPass := C.CString(pass)
	defer C.free(unsafe.Pointer(cIP))
	defer C.free(unsafe.Pointer(cUser))
	defer C.free(unsafe.Pointer(cPass))

	var deviceInfo C.NET_DVR_DEVICEINFO_V30
	loginID := int(C.NET_DVR_Login_V30(cIP, C.WORD(port), cUser, cPass, &deviceInfo))

	if loginID < 0 {
		errCode := C.NET_DVR_GetLastError()
		return 0, fmt.Errorf("SDK错误码: %d", errCode)
	}

	// 缓存登录信息
	sessionMap.Store(key, &SessionInfo{
		LoginID:    loginID,
		LastActive: time.Now(),
	})

	log.Printf("[登录成功] %s loginID=%d", key, loginID)
	return loginID, nil
}

// capturePicture 执行抓图
func capturePicture(loginID, channel int) ([]byte, error) {
	// JPEG 参数
	jpegPara := C.NET_DVR_JPEGPARA{
		wPicQuality: 0,    // 0-最好
		wPicSize:    0xff, // 自动分辨率
	}

	// 准备缓冲区（2MB）
	bufSize := C.DWORD(2 * 1024 * 1024)
	buffer := C.malloc(C.size_t(bufSize))
	defer C.free(buffer)

	var retLen C.DWORD
	res := C.NET_DVR_CaptureJPEGPicture_NEW(
		C.LONG(loginID),
		C.LONG(channel),
		&jpegPara,
		(*C.char)(buffer),
		bufSize,
		&retLen,
	)

	if res == 0 {
		errCode := C.NET_DVR_GetLastError()
		return nil, fmt.Errorf("SDK错误码: %d", errCode)
	}

	return C.GoBytes(buffer, C.int(retLen)), nil
}

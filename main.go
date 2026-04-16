package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"unsafe"
)

/*
#cgo LDFLAGS: -L./sdk/libs -lhcnetsdk
#cgo CFLAGS: -I./sdk/include

#include "HCNetSDK.h"
#include <stdlib.h>
*/
import "C"

// 缓存已登录的设备 ID
var sessionMap sync.Map

func main() {
	// 1. 初始化 SDK
	if C.NET_DVR_Init() == 0 {
		log.Fatal("SDK Init Failed")
	}
	defer C.NET_DVR_Cleanup()

	// 2. 设置连接超时
	C.NET_DVR_SetConnectTime(C.DWORD(2000), C.DWORD(1))

	// 3. HTTP 路由
	http.HandleFunc("/snapshot", snapshotHandler)
	http.HandleFunc("/health", healthHandler)

	log.Println("海康抓图服务启动在 :9876")
	log.Fatal(http.ListenAndServe(":9876", nil))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func snapshotHandler(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	port := r.URL.Query().Get("port")
	user := r.URL.Query().Get("user")
	pass := r.URL.Query().Get("pass")

	if ip == "" || user == "" || pass == "" {
		http.Error(w, "参数不全", http.StatusBadRequest)
		return
	}

	loginID := getLoginID(ip, port, user, pass)
	if loginID < 0 {
		http.Error(w, "登录失败", http.StatusUnauthorized)
		return
	}

	// 抓图参数设置
	jpegPara := C.NET_DVR_JPEGPARA{
		wPicQuality: 0,    // 0-最好
		wPicSize:    0xff, // 自动分辨率
	}

	// 准备 2MB 缓冲区
	bufSize := C.DWORD(2 * 1024 * 1024)
	buffer := C.malloc(C.size_t(bufSize))
	defer C.free(buffer)

	var retLen C.DWORD
	res := C.NET_DVR_CaptureJPEGPicture_NEW(
		C.LONG(loginID),
		C.LONG(1), // 通道号，通常是 1
		&jpegPara,
		(*C.char)(buffer),
		bufSize,
		&retLen,
	)

	if res == 0 {
		errCode := C.NET_DVR_GetLastError()
		http.Error(w, fmt.Sprintf("抓图失败: %d", errCode), http.StatusInternalServerError)
		return
	}

	// 将 C 内存转换为 Go byte slice 并返回给前端
	data := C.GoBytes(buffer, C.int(retLen))
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(data)
}

// 缓存登录逻辑：高性能的关键
func getLoginID(ip, port, user, pass string) int {
	key := fmt.Sprintf("%s-%s-%s", ip, user, pass)
	if id, ok := sessionMap.Load(key); ok {
		return id.(int)
	}

	cIP := C.CString(ip)
	cUser := C.CString(user)
	cPass := C.CString(pass)
	defer C.free(unsafe.Pointer(cIP))
	defer C.free(unsafe.Pointer(cUser))
	defer C.free(unsafe.Pointer(cPass))

	var deviceInfo C.NET_DVR_DEVICEINFO_V30
	loginID := C.NET_DVR_Login_V30(cIP, 8000, cUser, cPass, &deviceInfo)

	if loginID >= 0 {
		sessionMap.Store(key, int(loginID))
	}
	return int(loginID)
}

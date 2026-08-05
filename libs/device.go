package libs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
	"regexp"
	"strconv" 
	"github.com/yyszone/gua/adb"
	"github.com/yyszone/gua/services"
)

// ---------- Device：核心设备客户端 ----------

// Device 是 UIAutomator2 的核心客户端
// 封装了设备连接、UIAutomator2 服务管理、JSON-RPC 调用等功能
// 对应 Python 版本的 Device 类
type Device struct {
	// ADB 连接信息
	addr   string // ADB 服务器地址
	serial string // 设备序列号

	// UIAutomator2 服务配置
	serverPort int  // 设备端服务端口（默认 9008）
	debug      bool // 调试模式

	// 设备连接接口（通过 ADB 隧道直连）
	dev AdbDevice

	// JSON-RPC 调用器
	jsonrpc *JsonRpcWrapper

	// 设置
	settings *Settings

	// UIAutomator2 进程管理
	mu          sync.Mutex
	processConn net.Conn // 启动 UIAutomator 时的连接

	// 窗口尺寸缓存
	windowSizeCache [2]int
	
	infoCache     map[string]interface{}
    infoCacheTime time.Time
    infoCacheMu   sync.RWMutex
}

// NewDevice 创建一个新的 Device 客户端并启动 UIAutomator2 服务
// 通过 ADB 隧道直连设备，与 Python uiautomator2 完全一致
//
// serial: 设备序列号（如 "emulator-5554"）
// addr: 可选，ADB 服务器地址，不传则使用默认值 "127.0.0.1:5037"
//
// 创建后会自动执行：
//  1. 推送内嵌的 u2.jar 到设备（如果尚未存在）
//  2. 启动 UIAutomator2 服务
//  3. 等待服务就绪
func NewDevice(serial string, addr ...string) (*Device, error) {
    a := DefaultADBAddr
    if len(addr) > 0 && addr[0] != "" {
        a = addr[0]
    }

    // adb forward 本地 9009 → 设备 9008
    // 👉 【核心修改】将固定的 9009 修改为动态端口计算
    localPort := getUniqueLocalPort(serial) 
    if err := adb.ForwardPort(a, serial, localPort, DeviceServerPort); err != nil {
        // forward 失败则回退到 ADB 隧道
        log.Printf("adb forward 失败，回退到隧道模式: %v", err)
        return newDeviceWithTunnel(a, serial)
    }

    dev := &DirectDevice{LocalPort: localPort}
    d := &Device{
        addr:       a,
        serial:     serial,
        serverPort: DeviceServerPort,
        dev:        dev,
        settings:   NewSettings(),
    }
    d.jsonrpc = NewJsonRpcWrapper(func(method string, params interface{}, timeout float64) (json.RawMessage, error) {
        return d.jsonrpcCall(method, params, timeout)
    })

    if err := services.InstallServiceJar(a, serial, false); err != nil {
        return nil, fmt.Errorf("安装 u2.jar 失败: %w", err)
    }
    if err := d.StartUiautomator(); err != nil {
        return nil, err
    }
    return d, nil
}


// newDeviceWithTunnel 回退方案：使用 ADB 隧道
func newDeviceWithTunnel(a, serial string) (*Device, error) {
    dev := &AdbTunnelDevice{AdbAddr: a, Serial: serial}
    d := &Device{
        addr:       a,
        serial:     serial,
        serverPort: DeviceServerPort,
        dev:        dev,
        settings:   NewSettings(),
    }
    d.jsonrpc = NewJsonRpcWrapper(func(method string, params interface{}, timeout float64) (json.RawMessage, error) {
        return d.jsonrpcCall(method, params, timeout)
    })
    if err := services.InstallServiceJar(a, serial, false); err != nil {
        return nil, fmt.Errorf("安装 u2.jar 失败: %w", err)
    }
    if err := d.StartUiautomator(); err != nil {
        return nil, err
    }
    return d, nil
}

// NewDeviceWithoutStart 创建 Device 但不自动启动 UIAutomator2 服务
// 适用于服务已经在设备上运行的场景
//
// serial: 设备序列号
// addr: 可选，ADB 服务器地址，不传则使用默认值 "127.0.0.1:5037"
func NewDeviceWithoutStart(serial string, addr ...string) *Device {
	a := DefaultADBAddr
	if len(addr) > 0 && addr[0] != "" {
		a = addr[0]
	}

	dev := &AdbTunnelDevice{AdbAddr: a, Serial: serial}
	d := &Device{
		addr:       a,
		serial:     serial,
		serverPort: DeviceServerPort,
		dev:        dev,
		settings:   NewSettings(),
	}
	d.jsonrpc = NewJsonRpcWrapper(func(method string, params interface{}, timeout float64) (json.RawMessage, error) {
		return d.jsonrpcCall(method, params, timeout)
	})
	return d
}

// ---------- 设备属性 ----------

// Serial 返回设备序列号
func (d *Device) Serial() string {
	return d.serial
}

// Settings 返回设备的配置管理器
func (d *Device) Settings() *Settings {
	return d.settings
}

// SetDebug 设置调试模式
func (d *Device) SetDebug(debug bool) {
	d.debug = debug
}

// Debug 返回是否为调试模式
func (d *Device) Debug() bool {
	return d.debug
}

// ---------- UIAutomator2 服务管理 ----------

// StartUiautomator 启动 UIAutomator2 服务
// 如果服务已经在运行（/ping 响应 pong），则不会重复启动
func (d *Device) StartUiautomator() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 检查服务是否已经在运行
	if d.checkAlive() {
		return nil
	}

	// 启动 UIAutomator2 进程
	return d.launchAndWait()
}

// StopUiautomator 停止 UIAutomator2 服务
func (d *Device) StopUiautomator() {
	d.mu.Lock()
	if d.processConn != nil {
		d.processConn.Close()
		d.processConn = nil
	}
	d.mu.Unlock()

	// 等待服务退出
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !d.checkAlive() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Close 关闭设备连接，停止 UIAutomator2 服务
// ADB 隧道方案无需额外的端口清理
func (d *Device) Close() {
    d.StopUiautomator()
    switch td := d.dev.(type) {
    case *AdbTunnelDevice:
        td.Close()
    case *DirectDevice:
        td.Close()
    }
}
// ResetUiautomator 重启 UIAutomator2 服务
func (d *Device) ResetUiautomator() error {
	d.StopUiautomator()
	return d.StartUiautomator()
}

// checkAlive 通过 /ping 端点检查 UIAutomator2 服务是否存活
// 💡 升级为深度活性探针：同时利用 deviceInfo 检测 UiAutomation 状态，防止只通 HTTP 但 UiAutomation 已死的僵尸服务蒙混过关
func (d *Device) checkAlive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. 快速检查 HTTP 通信层
	resp, err := HttpRequest(ctx, d.dev, d.serverPort, "GET", "/ping", nil, 3.0, false)
	if err != nil || string(resp.Content) != "pong" {
		return false
	}

	// 2. 深度检查 UiAutomation 实际可用性 (调用最轻量的 deviceInfo JSON-RPC)
	// 直接使用底层的 JsonRpcCall 绕过任何上层重试逻辑，防止产生死循环
	_, err = JsonRpcCall(ctx, d.dev, d.serverPort, "deviceInfo", nil, 3.0, false)
	return err == nil
}
// launchAndWait 启动 UIAutomator2 进程并等待就绪
func (d *Device) launchAndWait() error {
	// 通过 ADB shell 启动 UIAutomator2
	conn, err := adb.ConnectToDevice(d.addr, d.serial, 15*time.Second)
	if err != nil {
		return &LaunchUiAutomationError{Message: "连接 ADB 失败", Output: err.Error()}
	}

	// 启动 UIAutomator2 服务进程
	cmd := "shell:CLASSPATH=/data/local/tmp/u2.jar app_process / com.wetest.uia2.Main"
	if err := adb.WriteAdbCmd(conn, cmd); err != nil {
		conn.Close()
		return &LaunchUiAutomationError{Message: "发送启动命令失败", Output: err.Error()}
	}
	status, err := adb.ReadStatus(conn)
	if err != nil {
		conn.Close()
		return &LaunchUiAutomationError{Message: "读取启动状态失败", Output: err.Error()}
	}
	if status != "OKAY" {
		conn.Close()
		return &LaunchUiAutomationError{Message: fmt.Sprintf("启动状态异常: %s", status)}
	}

	d.processConn = conn

	// 启动后台 goroutine 读取输出
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				output := string(buf[:n])
				if d.debug {
					log.Printf("[UIAutomator2] %s", output)
				}
				// 检查是否有 "already registered" 错误
				if strings.Contains(output, "already registered") {
					log.Printf("[UIAutomator2] 辅助功能服务已注册，需要重启")
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// 等待服务就绪
	return d.waitReady(30 * time.Second)
}

// waitReady 等待 UIAutomator2 服务就绪
func (d *Device) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.checkAlive() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return &LaunchUiAutomationError{Message: "服务启动超时"}
}

// ---------- JSON-RPC 调用 ----------

// jsonrpcCall 发送 JSON-RPC 调用，失败时自动重启 UIAutomator2 并重试
func (d *Device) jsonrpcCall(method string, params interface{}, timeout float64) (json.RawMessage, error) {
    ctx := context.Background()

    // t0 := time.Now()
    result, err := JsonRpcCall(ctx, d.dev, d.serverPort, method, params, timeout, d.debug)
    // log.Printf("[RPC] %s 耗时=%v err=%v", method, time.Since(t0), err)

    if err != nil {
        var uaErr *UiAutomationNotConnectedError
        var httpErr *HTTPError
        if errors.As(err, &uaErr) || errors.As(err, &httpErr) {
            log.Printf("UIAutomator2 服务异常，正在重启: %v", err)
            d.StopUiautomator()
            if startErr := d.StartUiautomator(); startErr != nil {
                return nil, startErr
            }
            return JsonRpcCall(ctx, d.dev, d.serverPort, method, params, timeout, d.debug)
        }
        return nil, err
    }
    return result, nil
}
// JsonRpc 返回 JSON-RPC 调用包装器
func (d *Device) JsonRpc() *JsonRpcWrapper {
	return d.jsonrpc
}

// ---------- Shell 命令 ----------

// ShellResponse Shell 命令的返回结果
type ShellResponse struct {
	Output   string // 命令输出
	ExitCode int    // 退出码（暂不支持精确返回）
}

// Shell 在设备上执行 Shell 命令
func (d *Device) Shell(cmdArgs ...string) (*ShellResponse, error) {
	cmd := strings.Join(cmdArgs, " ")

	conn, err := adb.ConnectToDevice(d.addr, d.serial, 10*time.Second)
	if err != nil {
		return nil, &AdbShellError{Message: fmt.Sprintf("连接失败: %v", err)}
	}
	defer conn.Close()

	out, err := adb.ExecShell(conn, cmd)
	if err != nil {
		return nil, &AdbShellError{Message: fmt.Sprintf("执行失败: %v", err)}
	}

	return &ShellResponse{
		Output:   string(out),
		ExitCode: 0,
	}, nil
}

// ---------- 设备信息 ----------

// Info 获取设备的 UI 信息（通过 JSON-RPC deviceInfo）
func (d *Device) Info() (map[string]interface{}, error) {
    d.infoCacheMu.RLock()
    if d.infoCache != nil && time.Since(d.infoCacheTime) < 2*time.Second {
        defer d.infoCacheMu.RUnlock()
        return d.infoCache, nil
    }
    d.infoCacheMu.RUnlock()

    raw, err := d.jsonrpc.Call("deviceInfo", nil, 10)
    if err != nil {
        return nil, err
    }
    var info map[string]interface{}
    if err := json.Unmarshal(raw, &info); err != nil {
        return nil, err
    }

    d.infoCacheMu.Lock()
    d.infoCache = info
    d.infoCacheTime = time.Now()
    d.infoCacheMu.Unlock()

    return info, nil
}

// 并发 Exists 检查
func (d *Device) ExistsAny(paramsList []map[string]interface{}) (int, bool) {
    type result struct {
        idx    int
        exists bool
    }
    ch := make(chan result, len(paramsList))
    for i, p := range paramsList {
        go func(idx int, params map[string]interface{}) {
            exists, _ := d.Exists(params)
            ch <- result{idx, exists}
        }(i, p)
    }
    for range paramsList {
        r := <-ch
        if r.exists {
            return r.idx, true
        }
    }
    return -1, false
}

// 在 device.go 加一个快速点击方法，不等 UI 稳定
// FastClick 快速点击，不等待 UI idle（比 RPC click 快 10 倍）
func (d *Device) FastClick(x, y int) error {
    _, err := d.Shell("input", "tap", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
    return err
}

// FastSwipe 快速滑动
func (d *Device) FastSwipe(fx, fy, tx, ty, durationMs int) error {
    _, err := d.Shell("input", "swipe",
        fmt.Sprintf("%d", fx), fmt.Sprintf("%d", fy),
        fmt.Sprintf("%d", tx), fmt.Sprintf("%d", ty),
        fmt.Sprintf("%d", durationMs))
    return err
}

// FastPress 快速按键
func (d *Device) FastPress(keycode string) error {
    _, err := d.Shell("input", "keyevent", strings.ToUpper(keycode))
    return err
}

// DeviceInfo 获取设备的硬件信息（通过 getprop）
func (d *Device) DeviceInfo() (map[string]interface{}, error) {
    props, err := adb.GetProp(d.addr, d.serial)
    if err != nil {
        return nil, err
    }
    return map[string]interface{}{
        "serial":  props["ro.serialno"],
        "sdk":     props["ro.build.version.sdk"],
        "brand":   props["ro.product.brand"],
        "model":   props["ro.product.model"],
        "arch":    props["ro.product.cpu.abi"],
        "version": props["ro.build.version.release"],
    }, nil
}

// WindowSize 获取设备屏幕尺寸（宽, 高）
func (d *Device) WindowSize() (int, int, error) {
	if d.windowSizeCache[0] > 0 {
		return d.windowSizeCache[0], d.windowSizeCache[1], nil
	}

	info, err := d.Info()
	if err != nil {
		return 0, 0, err
	}
	
	w := int(info["displayWidth"].(float64))
	h := int(info["displayHeight"].(float64))
	d.windowSizeCache = [2]int{w, h}
	return w, h, nil
}

// ---------- 基础操作 ----------

// Click 点击屏幕坐标。支持绝对像素(>=1)和相对比例(<1)
// libs/device.go

// Click 支持逻辑像素(>1)和比例(<1)
func (d *Device) GetPhysicalSize() (int, int, error) {
	// 执行 wm size 命令获取物理尺寸
	res, err := d.Shell("wm", "size")
	if err != nil {
		return 0, 0, err
	}
	// 输出格式通常为: "Physical size: 1080x2400"
	var w, h int
	_, err = fmt.Sscanf(strings.TrimSpace(res.Output), "Physical size: %dx%d", &w, &h)
	if err != nil {
		// 某些系统可能输出 "Override size: ..."
		_, err = fmt.Sscanf(strings.TrimSpace(res.Output), "Override size: %dx%d", &w, &h)
	}
	
	if w == 0 || h == 0 {
		return 0, 0, fmt.Errorf("无法解析物理分辨率: %s", res.Output)
	}
	return w, h, nil
}

// Click 修改版：支持比例和物理像素，且完全准确
func (d *Device) Click(x, y float64) error {
	d.operationDelay("click")

	targetX, targetY := x, y

	// 👉 如果是比例坐标，或者你想强制使用物理坐标系还原
	if x < 1.0 && y < 1.0 {
		// 关键：这里不能用 WindowSize，要用物理宽高
		pw, ph, err := d.GetPhysicalSize()
		if err == nil {
			targetX = x * float64(pw)
			targetY = y * float64(ph)
			// log.Printf("[Click] 物理还原: (%f, %f) * (%d, %d) = (%d, %d)", x, y, pw, ph, int(targetX), int(targetY))
		}
	}

	// 👉 核心：使用 FastClick (adb shell input tap)
	// 因为 input tap 接收的是物理坐标系，和截图完全匹配
	err := d.FastClick(int(targetX), int(targetY))
	
	d.operationDelayAfter("click")
	return err
}

// DoubleClick 双击屏幕坐标
func (d *Device) DoubleClick(x, y int, duration float64) error {
	if duration <= 0 {
		duration = 0.1
	}
	// 第一次按下抬起
	_, err := d.jsonrpc.Call("injectInputEvent", []interface{}{ActionDown, x, y, 0})
	if err != nil {
		return err
	}
	_, err = d.jsonrpc.Call("injectInputEvent", []interface{}{ActionUp, x, y, 0})
	if err != nil {
		return err
	}
	time.Sleep(time.Duration(duration * float64(time.Second)))
	// 第二次点击
	return d.Click(float64(x), float64(y)) 
}

// LongClick 长按屏幕坐标
// duration 为按住时间（秒），默认 0.5 秒
func (d *Device) LongClick(x, y int, duration float64) error {
	if duration <= 0 {
		duration = 0.5
	}
	d.operationDelay("click")
	_, err := d.jsonrpc.Call("click", []interface{}{x, y, int(duration * 1000)})
	d.operationDelayAfter("click")
	return err
}

// Swipe 从 (fx,fy) 滑动到 (tx,ty)
// steps: 滑动步数，每步约 5ms
func (d *Device) Swipe(fx, fy, tx, ty, steps int) error {
	if steps < 2 {
		steps = 2
	}
	d.operationDelay("swipe")
	_, err := d.jsonrpc.Call("swipe", []interface{}{fx, fy, tx, ty, steps})
	d.operationDelayAfter("swipe")
	return err
}

// SwipeWithDuration 按持续时间滑动
func (d *Device) SwipeWithDuration(fx, fy, tx, ty int, duration float64) error {
	steps := int(duration * 200)
	if steps < 2 {
		steps = ScrollSteps
	}
	return d.Swipe(fx, fy, tx, ty, steps)
}

// SwipePoints 多点连续滑动
// points 为坐标点列表 [[x1,y1], [x2,y2], ...]
// duration 为总持续时间（秒）
func (d *Device) SwipePoints(points [][2]int, duration float64) error {
	ppoints := make([]interface{}, 0, len(points)*2)
	for _, p := range points {
		ppoints = append(ppoints, p[0], p[1])
	}
	steps := int(duration / 0.005)
	_, err := d.jsonrpc.Call("swipePoints", []interface{}{ppoints, steps})
	return err
}

// Drag 将坐标从 (sx,sy) 拖拽到 (ex,ey)
func (d *Device) Drag(sx, sy, ex, ey int, duration float64) error {
	if duration <= 0 {
		duration = 0.5
	}
	d.operationDelay("drag")
	_, err := d.jsonrpc.Call("drag", []interface{}{sx, sy, ex, ey, int(duration * 200)})
	d.operationDelayAfter("drag")
	return err
}

// Press 按键操作
// key 可以是按键名称（如 "home", "back"）或按键代码
func (d *Device) Press(key string) error {
	d.operationDelay("press")
	_, err := d.jsonrpc.Call("pressKey", []interface{}{key})
	d.operationDelayAfter("press")
	return err
}

// PressKeyCode 按键代码操作
func (d *Device) PressKeyCode(keyCode int, meta ...int) error {
	d.operationDelay("press")
	params := []interface{}{keyCode}
	if len(meta) > 0 {
		params = append(params, meta[0])
	}
	_, err := d.jsonrpc.Call("pressKeyCode", params)
	d.operationDelayAfter("press")
	return err
}

// LongPress 长按按键
func (d *Device) LongPress(key string) error {
	d.operationDelay("press")
	_, err := d.Shell("input", "keyevent", "--longpress", strings.ToUpper(key))
	d.operationDelayAfter("press")
	return err
}

// ---------- 屏幕操作 ----------

// ScreenOn 唤醒屏幕
func (d *Device) ScreenOn() error {
	_, err := d.jsonrpc.Call("wakeUp", nil)
	return err
}

// ScreenOff 熄灭屏幕
func (d *Device) ScreenOff() error {
	_, err := d.jsonrpc.Call("sleep", nil)
	return err
}

// Screenshot 截取屏幕截图，返回 JPEG 图片的原始字节
func (d *Device) Screenshot() ([]byte, error) {
	raw, err := d.jsonrpc.Call("takeScreenshot", []interface{}{1, 80})
	if err != nil {
		return nil, err
	}
	// 结果是 base64 编码的字符串
	var base64Data string
	if err := json.Unmarshal(raw, &base64Data); err != nil {
		return nil, fmt.Errorf("解析截图数据失败: %w", err)
	}
	if base64Data == "" {
		return nil, fmt.Errorf("截图返回空数据")
	}

	// Base64 解码（使用标准库）
	decoded, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return nil, fmt.Errorf("Base64 解码失败: %w", err)
	}
	return decoded, nil
}

// ---------- 层级转储 ----------

// DumpHierarchy 转储当前窗口的 UI 层级 XML
// maxDepth 为最大递归深度，0 使用默认值
func (d *Device) DumpHierarchy(compressed bool, maxDepth int) (string, error) {
	if maxDepth <= 0 {
		maxDepth = d.settings.GetInt("max_depth")
		if maxDepth <= 0 {
			maxDepth = 50
		}
	}

	raw, err := d.jsonrpc.Call("dumpWindowHierarchy", []interface{}{compressed, maxDepth})
	if err != nil {
		return "", err
	}
	var content string
	if err := json.Unmarshal(raw, &content); err != nil {
		return "", err
	}
	if content == "" {
		return "", &HierarchyEmptyError{Message: "层级转储为空"}
	}
	if strings.Contains(content, `<hierarchy rotation="0" />`) {
		return "", &HierarchyEmptyError{Message: "层级转储为空（无子节点）"}
	}
	return content, nil
}

// ---------- 方向和旋转 ----------

// Orientation 获取当前屏幕方向
func (d *Device) Orientation() (string, error) {
	info, err := d.Info()
	if err != nil {
		return "", err
	}
	rotation := int(info["displayRotation"].(float64))
	for _, o := range Orientations {
		if o.Value == rotation {
			return o.Name, nil
		}
	}
	return "natural", nil
}

// SetOrientation 设置屏幕方向
// value 可以是 "natural"/"n"、"left"/"l"、"right"/"r"、"upsidedown"/"u"
func (d *Device) SetOrientation(value string) error {
	for _, o := range Orientations {
		if value == o.Name || value == o.Short || value == fmt.Sprintf("%d", o.Value) {
			_, err := d.jsonrpc.Call("setOrientation", []interface{}{o.Name})
			return err
		}
	}
	return fmt.Errorf("无效的方向值: %s", value)
}

// FreezeRotation 冻结/解冻屏幕旋转
func (d *Device) FreezeRotation(freeze bool) error {
	_, err := d.jsonrpc.Call("freezeRotation", []interface{}{freeze})
	return err
}

// ---------- 通知和快捷设置 ----------

// OpenNotification 打开通知栏
func (d *Device) OpenNotification() error {
	_, err := d.jsonrpc.Call("openNotification", nil)
	return err
}

// OpenQuickSettings 打开快捷设置
func (d *Device) OpenQuickSettings() error {
	_, err := d.jsonrpc.Call("openQuickSettings", nil)
	return err
}

// OpenURL 通过浏览器打开 URL
func (d *Device) OpenURL(url string) error {
	_, err := d.Shell("am", "start", "-a", "android.intent.action.VIEW", "-d", url)
	return err
}

// ---------- 剪贴板 ----------

// GetClipboard 获取剪贴板内容
func (d *Device) GetClipboard() (string, error) {
	raw, err := d.jsonrpc.Call("getClipboard", nil)
	if err != nil {
		return "", err
	}
	var text string
	json.Unmarshal(raw, &text)
	return text, nil
}

// SetClipboard 设置剪贴板内容
func (d *Device) SetClipboard(text string, label ...string) error {
	l := ""
	if len(label) > 0 {
		l = label[0]
	}
	_, err := d.jsonrpc.Call("setClipboard", []interface{}{l, text})
	return err
}

// ---------- Toast ----------

// GetLastToast 获取最后一个 Toast 消息
func (d *Device) GetLastToast() (string, error) {
	raw, err := d.jsonrpc.Call("getLastToast", nil)
	if err != nil {
		return "", err
	}
	var text string
	json.Unmarshal(raw, &text)
	return text, nil
}

// ClearToast 清除 Toast 消息
func (d *Device) ClearToast() error {
	_, err := d.jsonrpc.Call("clearLastToast", nil)
	return err
}

// MakeToast 在设备上显示 Toast 消息
func (d *Device) MakeToast(text string, durationMs float64) error {
	_, err := d.jsonrpc.Call("makeToast", []interface{}{text, durationMs * 1000})
	return err
}

// ---------- 等待超时 ----------

// ImplicitlyWait 设置默认等待超时
func (d *Device) ImplicitlyWait(seconds float64) error {
	return d.settings.Set("wait_timeout", seconds)
}

// WaitTimeout 获取当前的等待超时时间
func (d *Device) WaitTimeout() float64 {
	return d.settings.GetFloat64("wait_timeout")
}

// ---------- UiObject 选择器入口 ----------

// FindElement 根据选择器参数查找 UI 元素，返回 UiObject
func (d *Device) FindElement(params map[string]interface{}) (*UiObject, error) {
	sel, err := New(params)
	if err != nil {
		return nil, err
	}
	return NewUiObject(d, sel), nil
}

// By 通过任意选择器参数查找 UI 元素（便捷方法）
//
// 用法类似 Python 的 d(text="xxx", className="yyy")
//
//	d.By(libs.P{"text": "登录"}).Click()
//	d.By(libs.P{"resourceId": "com.example:id/btn", "clickable": true}).Click()
func (d *Device) By(params map[string]interface{}) *UiObject {
	sel := MustNew(params)
	return NewUiObject(d, sel)
}

// ByText 通过文本查找 UI 元素
//
//	d.ByText("向设备添加账号").Click()
func (d *Device) ByText(text string) *UiObject {
	return d.By(map[string]interface{}{"text": text})
}

// ByTextContains 通过包含的文本查找 UI 元素
//
//	d.ByTextContains("添加").Click()
func (d *Device) ByTextContains(text string) *UiObject {
	return d.By(map[string]interface{}{"textContains": text})
}

// ByResourceId 通过资源 ID 查找 UI 元素
//
//	d.ByResourceId("com.example:id/login_btn").Click()
func (d *Device) ByResourceId(id string) *UiObject {
	return d.By(map[string]interface{}{"resourceId": id})
}

// ByDescription 通过 contentDescription 查找 UI 元素
//
//	d.ByDescription("返回").Click()
func (d *Device) ByDescription(desc string) *UiObject {
	return d.By(map[string]interface{}{"description": desc})
}

// ByClassName 通过类名查找 UI 元素
//
//	d.ByClassName("android.widget.EditText").SetText("hello")
func (d *Device) ByClassName(className string) *UiObject {
	return d.By(map[string]interface{}{"className": className})
}

// P 是 map[string]interface{} 的别名，用于简化选择器参数书写
//
//	d.By(libs.P{"text": "确定", "clickable": true})
type P = map[string]interface{}

// ---------- 应用管理 ----------

// AppStart 启动应用
// packageName: 包名
// activity: Activity 名称（可选）
// stop: 是否先停止应用
func (d *Device) AppStart(packageName string, activity string, stop bool) error {
	// 1. 👉 启动前预检：使用 pm path 判断包是否存在
	// 这是 Android 最底层的命令，比 AppCurrent 快且稳
	resPath, err := d.Shell("pm", "path", packageName)
	if err != nil || !strings.Contains(resPath.Output, "package:") {
		return fmt.Errorf("设备未安装应用: %s", packageName)
	}

	if stop {
		d.AppStop(packageName)
	}

	var res *ShellResponse
	if activity == "" {
		// 使用 monkey 命令启动，并接住输出结果
		res, err = d.Shell("monkey", "-p", packageName, "-c",
			"android.intent.category.LAUNCHER", "1")
	} else {
		args := []string{
			"am", "start",
			"-a", "android.intent.action.MAIN",
			"-c", "android.intent.category.LAUNCHER",
			"-n", fmt.Sprintf("%s/%s", packageName, activity),
		}
		res, err = d.Shell(args...)
	}

	// 2. 👉 处理 ADB 连接层面的错误
	if err != nil {
		return err 
	}

	// 3. 👉 关键：解析 Shell 命令的打印输出
	// 即使命令退出码是 0，如果输出包含以下关键字，也代表启动失败
	output := res.Output
	if strings.Contains(output, "Error") || 
	   strings.Contains(output, "does not exist") || 
	   strings.Contains(output, "monkey aborted") {
		return fmt.Errorf("启动失败详情: %s", strings.TrimSpace(output))
	}

	return nil
}
// AppStop 停止应用
func (d *Device) AppStop(packageName string) error {
	_, err := d.Shell("am", "force-stop", packageName)
	return err
}

// AppClear 清除应用数据
func (d *Device) AppClear(packageName string) error {
	_, err := d.Shell("pm", "clear", packageName)
	return err
}

// AppUninstall 卸载应用
func (d *Device) AppUninstall(packageName string) (bool, error) {
	resp, err := d.Shell("pm", "uninstall", packageName)
	if err != nil {
		return false, err
	}
	return strings.Contains(resp.Output, "Success"), nil
}

// AppCurrent 获取当前前台应用信息
var reActivityComponent = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9_.]+)/([\.a-zA-Z][a-zA-Z0-9_./\$]+)`)

func (d *Device) AppCurrent() (string, error) {
    candidates := [][]string{
        {"dumpsys", "activity", "activities", "|", "grep", "topResumedActivity"},
        {"dumpsys", "activity", "activities", "|", "grep", "mResumedActivity"},
        {"dumpsys", "window", "windows", "|", "grep", "mCurrentFocus"},
        {"dumpsys", "window", "windows", "|", "grep", "mFocusedApp"},
    }
    for _, args := range candidates {
        resp, err := d.Shell(args...)
        if err != nil {
            continue
        }
        line := strings.TrimSpace(strings.SplitN(resp.Output, "\n", 2)[0])
        if line == "" {
            continue
        }
        if m := reActivityComponent.FindStringSubmatch(line); m != nil {
            return m[1], nil
        }
    }
    // 全量兜底（不依赖 grep，最兼容）
    resp, err := d.Shell("dumpsys", "activity", "activities")
    if err == nil {
        for _, line := range strings.Split(resp.Output, "\n") {
            line = strings.TrimSpace(line)
            if strings.Contains(line, "ActivityRecord") {
                if m := reActivityComponent.FindStringSubmatch(line); m != nil {
                    return m[1], nil
                }
            }
        }
    }
    return "", &DeviceError{Message: "无法获取前台应用信息"}
}

func (d *Device) AppWait(packageName string, timeout float64, front bool) (int, error) {
    if timeout <= 0 {
        timeout = 20.0
    }
    deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
    for time.Now().Before(deadline) {
        if front {
            pkg, err := d.AppCurrent()
            if err == nil && pkg == packageName {
                pid := d.pidOfApp(packageName)
                if pid > 0 {
                    return pid, nil
                }
            }
        } else {
            pid := d.pidOfApp(packageName)
            if pid > 0 {
                return pid, nil
            }
        }
        time.Sleep(1 * time.Second)
    }
    return 0, nil
}
// pidOfApp 获取应用的进程 ID
func (d *Device) pidOfApp(packageName string) int {
	resp, err := d.Shell("ps", "-A")
	if err != nil {
		return 0
	}
	output := resp.Output
	if len(strings.TrimSpace(output)) <= 1 {
		resp, err = d.Shell("ps")
		if err != nil {
			return 0
		}
		output = resp.Output
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 9 && fields[len(fields)-1] == packageName {
			pid := 0
			fmt.Sscanf(fields[1], "%d", &pid)
			return pid
		}
	}
	return 0
}

// ---------- 文件操作 ----------

// Push 推送文件到设备
func (d *Device) Push(localPath, remotePath string) error {
	_, err := adb.PushFile(d.addr, d.serial, localPath, remotePath, 0644, d.debug)
	return err
}

// ---------- 辅助方法 ----------

// operationDelayHelper 通用操作延迟辅助函数
// isBefore 为 true 时取操作前延迟，否则取操作后延迟
func (d *Device) operationDelayHelper(operation string, isBefore bool) {
	methods := d.settings.GetStringSlice("operation_delay_methods")
	for _, m := range methods {
		if m == operation {
			before, after := d.settings.GetOperationDelay()
			delay := after
			if isBefore {
				delay = before
			}
			if delay > 0 {
				time.Sleep(time.Duration(delay * float64(time.Second)))
			}
			return
		}
	}
}

// operationDelay 操作前延迟
func (d *Device) operationDelay(operation string) {
	d.operationDelayHelper(operation, true)
}

// operationDelayAfter 操作后延迟
func (d *Device) operationDelayAfter(operation string) {
	d.operationDelayHelper(operation, false)
}

// ---------- 存在性检查 ----------

// Exists 检查匹配选择器参数的 UI 元素是否存在
func (d *Device) Exists(params map[string]interface{}) (bool, error) {
	obj, err := d.FindElement(params)
	if err != nil {
		return false, err
	}
	return obj.Exists()
}

// ClearText 清除输入框文本
func (d *Device) ClearText() error {
	_, err := d.jsonrpc.Call("clearInputText", nil)
	return err
}

// Keyevent 发送按键事件
func (d *Device) Keyevent(key string) error {
	_, err := d.Shell("input", "keyevent", strings.ToUpper(key))
	return err
}

// WlanIP 获取设备 WLAN IP 地址
func (d *Device) WlanIP() (string, error) {
	resp, err := d.Shell("ip", "addr", "show", "wlan0")
	if err != nil {
		return "", err
	}
	lines := strings.Split(resp.Output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "inet ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ip := strings.Split(parts[1], "/")[0]
				return ip, nil
			}
		}
	}
	return "", nil
}

// Unlock 解锁屏幕（从左下滑到右上）
func (d *Device) Unlock() error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	screenOn, _ := info["screenOn"].(bool)
	if !screenOn {
		d.Keyevent("POWER")
		w, h, err := d.WindowSize()
		if err != nil {
			return err
		}
		return d.Swipe(int(float64(w)*0.1), int(float64(h)*0.9),
			int(float64(w)*0.9), int(float64(h)*0.1), ScrollSteps)
	}
	return nil
}

// 👉 【新增】根据设备 IP 或序列号，生成或寻找一个宿主机上不冲突的唯一本地转发端口
func getUniqueLocalPort(serial string) int {
	defaultPort := 9009

	// 1. 尝试去掉端口号，例如 "192.168.31.120:5555" -> "192.168.31.120"
	host := serial
	if idx := strings.Index(serial, ":"); idx != -1 {
		host = serial[:idx]
	}

	// 2. 如果是标准的 IPv4 格式，提取最后一段作为偏移量 (100% 保证并发 IP 间不冲突)
	parts := strings.Split(host, ".")
	if len(parts) == 4 {
		lastOctet, err := strconv.Atoi(parts[3])
		if err == nil {
			// 例如 182 映射到 20182，158 映射到 20158
			return 20000 + lastOctet
		}
	}

	// 3. 如果不是 IP（比如 USB 序列号 "988a1b2c"），则动态申请一个系统当前可用的空闲端口
	return getFreePort(defaultPort)
}

// 👉 【新增】获取当前系统随机空闲端口的兜底函数
func getFreePort(defaultPort int) int {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return defaultPort
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return defaultPort
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
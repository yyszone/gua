package libs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"sync"   
	"github.com/yyszone/gua/adb"
)

// ---------- 外部接口和类型定义 ----------

// AdbDevice 定义了设备连接接口
type AdbDevice interface {
	// CreateConnection 建立到设备的 TCP 连接
	// network 通常为 "tcp"，port 为设备上服务监听端口
	
	CreateConnection(network string, port int) (net.Conn, error)
	GetTransport(devicePort int) *http.Transport
}

// AdbTunnelDevice 通过 ADB 隧道直连设备（与 Python uiautomator2 完全一致）
// 每次 CreateConnection 会建立一条新的 ADB 隧道，无需 adb forward，无需端口管理
// 等同于 Python 中 AdbHTTPConnection 继承 HTTPConnection 并重写 connect() 的方案
type AdbTunnelDevice struct {
	AdbAddr string // ADB 服务器地址
	Serial  string // 设备序列号
	once      sync.Once
	transportOnce sync.Once  
    transport *http.Transport  // 复用
}
type DirectDevice struct {
    LocalPort int
    once      sync.Once
    transport *http.Transport
}

func (d *DirectDevice) CreateConnection(network string, port int) (net.Conn, error) {
    return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", d.LocalPort))
}

// CreateConnection 建立到设备指定端口的 ADB 隧道连接
// CreateConnection 建立新的 ADB 隧道（供 Transport.DialContext 使用）

func (d *AdbTunnelDevice) CreateConnection(network string, port int) (net.Conn, error) {
    return adb.CreateTunnel(d.AdbAddr, d.Serial, port)
}
func (d *DirectDevice) GetTransport(devicePort int) *http.Transport {
    d.once.Do(func() {
        d.transport = &http.Transport{
            DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", d.LocalPort))
            },
            DisableKeepAlives:   false,
            MaxIdleConns:        4,
            MaxIdleConnsPerHost: 4,
            IdleConnTimeout:     60 * time.Second,
        }
    })
    return d.transport
}

func (d *DirectDevice) Close() {
    if d.transport != nil {
        d.transport.CloseIdleConnections()
    }
}
// GetTransport 返回复用的 http.Transport（懒初始化）
func (d *AdbTunnelDevice) GetTransport(devicePort int) *http.Transport {
    d.transportOnce.Do(func() {
        d.transport = &http.Transport{
            DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                return d.CreateConnection("tcp", devicePort)
            },
            // 允许复用连接
            DisableKeepAlives:   false,
            MaxIdleConns:        4,
            MaxIdleConnsPerHost: 4,
            IdleConnTimeout:     60 * time.Second,
        }
    })
    return d.transport
}

// Close 关闭连接池，释放所有空闲连接
func (d *AdbTunnelDevice) Close() {
    if d.transport != nil {
        d.transport.CloseIdleConnections()
    }
}

// HTTPResponse 封装 HTTP 响应数据
type HTTPResponse struct {
	Content []byte // 响应体内容
	Status  int    // HTTP 状态码
	Reason  string // 状态描述
}

// JSON 将响应体解析为指定的结构体
func (r *HTTPResponse) JSON(v interface{}) error {
	return json.Unmarshal(r.Content, v)
}

// Text 返回响应体的文本内容
func (r *HTTPResponse) Text() string {
	return string(r.Content)
}

// ---------- 自定义错误类型 ----------

var (
	// ErrHTTPTimeout 表示 HTTP 请求超时
	ErrHTTPTimeout = errors.New("HTTP 请求超时")
	// ErrHTTPFailed 表示 HTTP 请求失败
	ErrHTTPFailed = errors.New("HTTP 请求失败")
)

// ---------- HttpRequest：通过标准 HTTP 客户端发送请求 ----------

// HttpRequest 向设备端 UIAutomator2 服务发送 HTTP 请求
// 使用 Go 标准 http.Client + 自定义 ADB 隧道 Dialer，与 Python uiautomator2 完全一致
//
// 工作原理（与 Python 的 AdbHTTPConnection 等价）：
//  1. http.Transport 的 DialContext 会为每个请求建立一条新的 ADB 隧道
//  2. 标准 http.Client 在这条隧道上发送完整的 HTTP/1.1 请求
//  3. 请求完成后隧道自动关闭，无需额外清理
func HttpRequest(ctx context.Context, dev AdbDevice, devicePort int, method, path string,
    data map[string]interface{}, timeoutSecs float64, printRequest bool) (*HTTPResponse, error) {

    if timeoutSecs <= 0 {
        timeoutSecs = 10.0
    }

    // 调试打印（不变）
    if printRequest {
        now := time.Now().Format("15:04:05.000")
        url := fmt.Sprintf("http://<adb-tunnel>:%d%s", devicePort, path)
        if data != nil {
            b, _ := json.Marshal(data)
            fmt.Printf("# HTTP 超时=%.3f\n%s $ curl -X %s %s -d '%s'\n", timeoutSecs, now, method, url, string(b))
        } else {
            fmt.Printf("# HTTP 超时=%.3f\n%s $ curl -X %s %s\n", timeoutSecs, now, method, url)
        }
    }

    url := fmt.Sprintf("http://127.0.0.1:%d%s", devicePort, path)
    var body io.Reader
    if data != nil {
        b, err := json.Marshal(data)
        if err != nil {
            return nil, fmt.Errorf("JSON 编码失败: %w", err)
        }
        body = bytes.NewReader(b)
    }

    req, err := http.NewRequestWithContext(ctx, method, url, body)
    if err != nil {
        return nil, fmt.Errorf("创建 HTTP 请求失败: %w", err)
    }
    req.Header.Set("Content-Type", "application/json; charset=utf-8")

    // ✅ 复用 Transport，不再每次 new
    client := &http.Client{
        Transport: dev.GetTransport(devicePort),
        Timeout:   time.Duration(timeoutSecs * float64(time.Second)),
    }

    resp, err := client.Do(req)
    if err != nil {
        var netErr net.Error
        if errors.As(err, &netErr) && netErr.Timeout() {
            return nil, fmt.Errorf("%w: %v", ErrHTTPTimeout, err)
        }
        return nil, fmt.Errorf("%w: %v", ErrHTTPFailed, err)
    }
    defer resp.Body.Close()

    content, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, fmt.Errorf("读取响应体失败: %w", err)
    }

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("HTTP 请求失败: %d %s", resp.StatusCode, resp.Status)
    }

    response := &HTTPResponse{
        Content: content,
        Status:  resp.StatusCode,
        Reason:  resp.Status,
    }

    if printRequest {
        now := time.Now().Format("15:04:05.000")
        fmt.Printf("%s 响应 >>>\n%s\n<<< 结束\n\n", now, strings.TrimRight(response.Text(), "\n"))
    }

    return response, nil
}

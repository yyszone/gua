针对您的代码库 `github.com/yyszone/gua`，我为您重新整理并编写了一份最为完整、专业的 `README.md`。这份文档涵盖了从底层协议原理到高层业务调用的所有功能。

---

# Gua (Go Mobile UIAutomator)

`Gua` 是一个基于纯 Go 语言实现的 Android UI 自动化库。它参考了 Python `uiautomator2` 的连接架构，通过 **ADB 隧道协议** 直接与设备通信，无需 `adb forward`，不占用本地端口，具备极高的稳定性和并发支持能力。

## ✨ 功能特性

- **🚀 零依赖部署**：通过 `go:embed` 将 `u2.jar` 和 `APK` 内置于二进制文件中，一键运行，自动安装。
- **🔗 底层 ADB 协议**：纯 Go 实现的 ADB 客户端，支持设备发现、隧道建立、文件同步 (Sync Push)。
- **🎯 强大的选择器**：
    - **Selector**: 支持 Text, ID, Class, Description 等正则/精确匹配。
    - **XPath**: 支持标准 XPath 1.0 语法，轻松处理复杂层级。
    - **OCR**: 集成 Tesseract OCR，支持图像文字识别与坐标定位。
- **🖱️ 深度交互**：支持点击、长按、双击、多点滑动、拖拽、快速操作 (FastClick)。
- **⌨️ 输入法集成**：内置 AdbKeyboard，完美支持中文输入和特殊动作 (Done/Search)。
- **🛡️ 智能监控**：Watcher 机制自动拦截系统权限弹窗、广告、升级提示。
- **📱 完善的系统控制**：截图、剪贴板、App 管理、Shell 命令、屏幕旋转、电池信息等。

---

## 📦 安装

```bash
go get github.com/yyszone/gua
```

---

## 📖 完整使用指南

### 1. 建立连接
```go
import "github.com/yyszone/gua/libs"

// 连接设备并自动初始化服务（推送 jar/apk、启动服务）
d, err := libs.NewDevice("emulator-5554") 
defer d.Close()

// 如果服务已在运行，可跳过初始化步骤
// d := libs.NewDeviceWithoutStart("emulator-5554")
```

### 2. UI 元素定位

#### A. 链式选择器 (Selector)
```go
// 通过 Text 查找
d.ByText("设置").Click()

// 多条件组合 (使用 libs.P 别名简化)
d.By(libs.P{"resourceId": "com.android.systemui:id/back", "clickable": true}).Click()

// 模糊匹配
d.ByTextContains("登录").Wait(true, 10.0)

// 查找子元素
parent := d.ByClassName("android.widget.ListView")
child, _ := parent.Child(libs.P{"text": "存储"})
```

#### B. XPath 定位
```go
// 查找 text 为“支付宝”的节点
el, _ := d.FindByXPath(`//node[@text="支付宝"]`)

// 组合逻辑查找
els, _ := d.FindAllByXPath(`//node[contains(@resource-id, "icon") and @clickable="true"]`)
```

#### C. OCR 文字识别定位
```go
// 识别屏幕上的文字并获取坐标
res, _ := d.OCR(libs.OCROptions{Lang: "chi_sim", BinarizeNum: 210})
for _, r := range res.Regions {
    if r.Text == "开始游戏" {
        d.Click(int(r.CenterX), int(r.CenterY))
    }
}
```

### 3. UI 操作交互

#### 点击与手势
```go
d.Click(500, 500)                 // 坐标点击
d.LongClick(500, 500, 2.0)        // 长按坐标 2 秒
d.Swipe(100, 500, 100, 100, 50)   // 坐标滑动
d.Drag(100, 100, 500, 500, 0.5)   // 拖拽

// Fast 系列：不等待 UI 稳定，速度提升 10 倍（通过 shell 直接发送）
d.FastClick(500, 500)
d.FastSwipe(100, 500, 100, 100, 200)
```

#### 元素级操作
```go
obj := d.ByText("登录")
obj.SetText("myusername")         // 输入文本
obj.ClearText()                   // 清空文本
obj.ScrollToEnd(true, 10, 50)     // 滚动到底部
```

### 4. 输入法与中文输入
```go
im := libs.NewInputMethod(d)
im.SendKeys("你好 Gua 自动化")      // 自动切换 AdbKeyboard 输入中文
im.SendAction("search")           // 模拟键盘点击“搜索”
```

### 5. App 进程管理
```go
d.AppStart("com.tencent.mm", "", true) // 启动微信，stop=true 表示先强杀
d.AppStop("com.tencent.mm")            // 停止应用
d.AppClear("com.tencent.mm")           // 清除数据
current, _ := d.AppCurrent()           // 获取当前包名和 Activity
```

### 6. 弹窗自动监控 (Watcher)
```go
w := libs.NewWatchContext(d, true) // 启用内置的“允许”、“确定”等规则
w.WhenText("不再提醒").Click()      // 自定义规则
w.Start()                          // 异步后台监控
// ... 执行复杂逻辑
w.Stop()
```

### 7. 系统与文件操作
```go
// 截图并保存
img, _ := d.Screenshot()
os.WriteFile("shot.jpg", img, 0644)

// 剪贴板
d.SetClipboard("Gua-Auth")
text, _ := d.GetClipboard()

// 文件推送 (ADB Sync 协议)
d.Push("local.txt", "/sdcard/remote.txt")

// 执行 ADB Shell 命令
res, _ := d.Shell("getprop", "ro.product.model")
fmt.Println(res.Output)
```

---

## 🏗️ 架构参考

### ADB 隧道 (Tunneling)
本项目连接设备的核心代码位于 `adb/connect.go` 的 `CreateTunnel` 方法。它直接模拟 ADB 协议握手，通过 `host:transport` 路由到设备，随后建立直连 TCP 管道。
- **优点**：不需要在本地机器开启 9008 端口转发，彻底解决端口冲突和断连后的资源残留问题。

### UIAutomator2 服务
通过 `assets/` 下的 `u2.jar` 和 `APK`，在 Android 端开启一个基于 Jetty 的 HTTP 服务，监听 9008 端口。Go 端的所有 UI 操作最终都会转化为 JSON-RPC 2.0 请求发送给设备。

---

## 📂 目录结构说明

- `adb/`: 基础 ADB 协议实现（连接、读取状态、文件同步、隧道构建）。
- `libs/`: 核心业务逻辑（设备类、元素类、XPath 引擎、OCR、输入法、监控器）。
- `services/`: 服务部署逻辑（检查环境、安装 jar 和 apk）。
- `assets/`: 静态资源文件（由 `go:embed` 引入）。
- `cmd/`: 演示 Demo 样例。

---

## ⚠️ 依赖须知

1. **环境**: 需要 Android 5.0+ 设备。
2. **OCR**: 使用 OCR 功能需在本地安装 [Tesseract-OCR](https://github.com/tesseract-ocr/tesseract) 并配置到 PATH。
3. **ADB**: 宿主机需运行 `adb server`（默认 5037 端口）。

## 📜 许可证

本项目采用 **MIT** 许可证开源。


针对您的代码库 `github.com/yyszone/gua`，我为您重新整理并编写了一份最为完整、专业的 `README.md`。这份文档涵盖了从底层协议原理到高层业务调用的所有功能。

---

# Gua (Go Mobile UIAutomator)

`Gua` 是一个基于纯 Go 语言实现的 Android UI 自动化库。它参考了 Python `uiautomator2` 的连接架构，通过纯 Go ADB 客户端与设备通信，默认尝试动态本地端口转发；转发不可用时自动回退到 ADB 隧道模式，避免依赖固定端口并降低端口冲突。

## ✨ 功能特性

- **🚀 零依赖部署**：通过 `go:embed` 将 `u2.jar` 和 `APK` 内置于二进制文件中，一键运行，自动安装。
- **🔗 底层 ADB 协议**：纯 Go 实现的 ADB 客户端，支持设备发现、隧道建立、文件同步 (Sync Push)。
- **🎯 强大的选择器**：
    - **Selector**: 支持 Text, ID, Class, Description 等正则/精确匹配。
    - **XPath**: 支持标准 XPath 1.0 语法，轻松处理复杂层级。
    - **OCR**: 集成 Tesseract OCR，支持图像文字识别与坐标定位。
    - **图像模板找图**：基于 SAD 相似度匹配，支持透明 PNG、多模板优先级、同屏多目标查找和模板缓存，无需 OCR。
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
if err != nil {
    log.Fatal(err)
}
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

#### D. 图像模板找图（Image Matching）

Gua 内置图像模板匹配功能，可以直接在设备截图中查找按钮、图标、图片或其他视觉元素。它不依赖 Tesseract OCR，适合文字识别不稳定、控件没有可用 `resourceId`，或者需要按图标定位的场景。

**1）准备模板图片**

默认模板目录是程序当前工作目录下的 `find_pic`，支持 PNG、JPG、JPEG 和 GIF（GIF 使用解码后的首帧）。文件名按前缀匹配，同一个前缀可以放多张不同分辨率或状态的模板变体。

```text
find_pic/
├── login.png
├── confirm.jpg
└── 应用宝/
    └── 应用宝_领积分.png
```

当模板名称包含下划线，且 `find_pic` 下存在以下划线前半部分命名的子目录时，Gua 会优先在该子目录中查找。例如 `应用宝_领积分` 会优先查找 `find_pic/应用宝/` 中以 `应用宝_领积分` 开头的图片文件。

建议模板图片从目标设备的实际截图中裁剪，尽量保持相同分辨率、缩放比例、主题和显示状态。不同分辨率可以为同一个前缀准备多张模板变体。

**2）查找单个模板**

```go
// 查找 login.png，阈值 0.85，返回像素坐标。
// 最后一个参数 1 表示只截屏匹配一次。
x, y, index := d.FindImage("login", 0.85, false, 1)
if index != -1 {
    fmt.Printf("找到 login，中心坐标：(%.0f, %.0f)\n", x, y)
    _ = d.Click(x, y)
}
```

**3）按优先级查找多个模板**

```go
images := []string{"login", "confirm", "close"}

// 按 images 的顺序确定优先级。
// 如果 login 和 confirm 都命中，优先返回 login。
x, y, index := d.FindImage(images, 0.85, false, 1)
if index >= 0 {
    fmt.Printf("命中模板：%s，坐标：(%.0f, %.0f)\n", images[index], x, y)
    _ = d.Click(x, y)
}
```

`FindImage` 参数说明：

- `targetNames`：模板名称字符串，或按优先级排列的 `[]string`。
- `threshold`：相似度阈值，范围为 `0.0~1.0`，例如 `0.90`。
- `usePerc`：`false` 返回像素坐标；`true` 返回屏幕宽高比例坐标（0~1）。
- `maxRetry`：可选重试次数；不传或传入非正数时默认重试 30 次。

返回值是匹配区域中心点的 `x`、`y` 和命中模板在 `targetNames` 中的索引。未找到时返回 `(-1, -1, -1)`。

**4）查找同一模板的所有位置**

例如一个页面上有多个相同图标，可以一次性找出全部位置：

```go
// usePerc=true，返回 0~1 的比例坐标。
// maxResults=0 表示不限制结果数量。
points, err := d.FindImageMulti("应用宝_领积分", 0.93, true, 0)
if err != nil {
    log.Printf("找图失败：%v", err)
    return
}

for _, p := range points {
    fmt.Printf("找到坐标：x=%.4f, y=%.4f, score=%.3f\n", p.X, p.Y, p.Score)
    // Device.Click 支持比例坐标，可以直接传入。
    _ = d.Click(p.X, p.Y)
    time.Sleep(100 * time.Millisecond)
}
```

`FindImageMulti` 参数说明：

- `targetName`：模板文件名前缀；同名前缀的图片变体都会参与匹配。
- `threshold`：相似度阈值，范围为 `0.0~1.0`。
- `usePerc`：是否返回屏幕比例坐标。
- `maxResults`：最多返回多少个结果，`<=0` 表示不限制。
- `maxRetry`：可选重试次数；不传或传入非正数时默认重试 1 次。

返回的 `libs.MatchPoint` 包含 `X`、`Y` 和 `Score`。结果按屏幕从上到下、从左到右排列；不同模板变体命中的同一位置会进行去重。没有找到匹配时返回空切片和 `nil`；模板目录、图片解码或截图发生错误时会返回错误。

**5）修改模板目录和管理缓存**

默认使用当前工作目录下的 `find_pic`。也可以在程序启动时自定义目录：

```go
// 相对路径以程序当前工作目录为基准；也可以传入绝对路径。
if err := libs.SetImageTemplateDir("./assets/my_templates"); err != nil {
    log.Fatal(err)
}

fmt.Println("当前模板目录：", libs.GetImageTemplateDir())
```

模板图片缓存会根据文件大小和修改时间自动刷新。目录文件列表缓存时间为 30 秒；新增或删除模板后，如果希望立即刷新，可以调用：

```go
libs.ClearImageTemplateCache()
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
current, _ := d.AppCurrent()            // 获取当前包名和 Activity
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

### ADB 连接与隧道回退
设备连接代码位于 `adb/`。`NewDevice` 会优先尝试动态分配本地端口并建立 ADB 转发；转发失败时自动回退到 `CreateTunnel`，通过 ADB 协议握手和 `host:transport` 建立设备隧道。
- **优点**：不依赖固定本地端口；转发不可用时仍可使用隧道方式连接设备。

### UIAutomator2 服务
通过 `assets/` 下的 `u2.jar` 和 APK，在 Android 端开启一个基于 Jetty 的 HTTP 服务，监听 9008 端口。Go 端的所有 UI 操作最终都会转化为 JSON-RPC 2.0 请求发送给设备。

---

## 📂 目录结构说明

- `adb/`: 基础 ADB 协议实现（连接、读取状态、文件同步、隧道构建）。
- `libs/`: 核心业务逻辑（设备类、元素类、XPath 引擎、OCR、图像模板匹配、输入法、监控器）。
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

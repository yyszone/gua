package main

import (
	"fmt"
	"time"

	"github.com/yyszone/gua/libs"
)

func main() {
	// 1. 初始化设备 (填入你真实的设备 Serial 或局域网 IP)
	d, err := libs.NewDevice("192.168.31.182:5555")
	if err != nil {
		panic(err)
	}
	defer d.Close()

	// 等待设备 UI 稳定
	time.Sleep(2 * time.Second)

	fmt.Println("========================================")
	fmt.Println("--- 开始执行 OCR 屏幕文本识别 ---")

	// 2. 调用 OCR 接口
	// (环境要求：系统必须已安装 Tesseract，即命令行可以运行 `tesseract`)
	// (Linux: sudo apt install tesseract-ocr tesseract-ocr-chi-sim)
	// (Windows: 下载安装包并将 Tesseract 目录加入系统环境变量 Path)
	ocrResult, err := d.OCR(libs.OCROptions{
		Lang:                   "chi_sim", // 简体中文
		PSM:                    3,         // 自动检测多行
		BinarizeNum:            210,       // 二值化过滤图像噪点，有助于提高准确率
		HorizontalGapThreshold: 30,        // 相近文字自动合并成一个词块的间隙像素
		VerticalGapThreshold:   10,
		XYPercentage:           false,     // 设为 false 表示输出具体像素，方便使用 d.Click()
	})

	if err != nil {
		fmt.Printf("OCR 识别发生错误: %v\n", err)
		return
	}

	// 3. 打印解析到的全部文本（按设置的 Padding 拼合）
	fmt.Printf("✅ 屏幕全部解析文本预览: \n%s\n\n", ocrResult.AllText)

	// 4. 遍历打印出所有连通区域以及其坐标
	var targetRegion *libs.OCRRegion

	fmt.Println("🔎 找到的文字块详细列表:")
	for i, region := range ocrResult.Regions {
		fmt.Printf("   [%d] 文本: '%s', 置信度: %.2f, 像素中心点: (%.1f, %.1f), 边框: %v\n",
			i, region.Text, region.Accuracy, region.CenterX, region.CenterY, region.Box)

		// 模拟：假如我们想通过 OCR 找到“支付宝”并点击它
		if region.Text == "支付宝" || region.Text == "微信" { // 这里写你想要的搜索词
			targetRegion = &region
		}
	}

	fmt.Println("========================================")

	// 5. 点击通过 OCR 找到的目标区域
	if targetRegion != nil {
		fmt.Printf("👆 准备点击目标: [%s] (坐标: X=%d, Y=%d)\n", targetRegion.Text, int(targetRegion.CenterX), int(targetRegion.CenterY))

		// 因为设置了 XYPercentage: false，所以这里 centerX 和 centerY 都是实际像素点
		err = d.Click(targetRegion.CenterX, targetRegion.CenterY)
		if err != nil {
			fmt.Printf("点击失败: %v\n", err)
		} else {
			fmt.Println("✅ 点击成功!")
		}
	} else {
		fmt.Println("⚠️ 在屏幕中未能通过 OCR 找到期望的目标词汇")
	}
}
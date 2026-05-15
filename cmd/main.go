package main

import (
	"fmt"
	"time"

	"github.com/yyszone/gua/libs"
)

func main() {
	d, err := libs.NewDevice("192.168.31.182:5555")
	if err != nil {
		panic(err)
	}
	defer d.Close()

	// 先休眠几秒等待屏幕稳定（因为 XPath 是瞬时抓取屏幕层级）
	time.Sleep(2 * time.Second)

	fmt.Println("========================================")
	// -------------------------
	// XPath 案例 1：精确匹配文本并点击 (等价于 d.ByText)
	// -------------------------
	fmt.Println("--- 案例 1：XPath 精确文本查找 ---")
	xpEl1, err := d.FindByXPath(`//node[@text="支付宝"]`)
	if err != nil {
		fmt.Println("未找到支付宝:", err)
	} else {
		fmt.Printf("✅ 找到元素，文本: '%s', ResourceId: '%s'\n", xpEl1.Text(), xpEl1.ResourceId())
		
		// 点击该元素
		if err := xpEl1.Click(); err != nil {
			fmt.Println("XPath 点击失败:", err)
		} else {
			fmt.Println("XPath 点击支付宝成功")
		}
	}

	fmt.Println("========================================")
	// -------------------------
	// XPath 案例 2：多条件组合匹配 (使用 and)
	// -------------------------
	fmt.Println("--- 案例 2：XPath 多条件组合 ---")
	// 查找 text="支付宝" 并且 class 是 TextView 的节点
	xpath2 := `//node[@text="支付宝" and @class="android.widget.TextView"]`
	xpEl2, err := d.FindByXPath(xpath2)
	if err == nil {
		x, y, w, h, _ := xpEl2.Bounds()
		fmt.Printf("✅ 找到组合条件元素，坐标范围: X=%d, Y=%d, 宽=%d, 高=%d\n", x, y, w, h)
	}

	fmt.Println("========================================")
	// -------------------------
	// XPath 案例 3：模糊匹配 (contains 包含关键字)
	// -------------------------
	fmt.Println("--- 案例 3：XPath 模糊匹配 ---")
	// 查找 resource-id 包含 "icon_title" 的节点
	xpath3 := `//node[contains(@resource-id, "icon_title")]`
	xpEl3, err := d.FindByXPath(xpath3)
	if err == nil {
		fmt.Printf("✅ 找到模糊匹配元素，文本: '%s', ResourceId: '%s'\n", xpEl3.Text(), xpEl3.ResourceId())
	}

	fmt.Println("========================================")
	// -------------------------
	// XPath 案例 4：查找屏幕上所有的匹配项 (FindAllByXPath)
	// -------------------------
	fmt.Println("--- 案例 4：XPath 查找全部节点 ---")
	// 查找屏幕上所有 clickable="true" (可点击) 的节点
	xpath4 := `//node[@clickable="true"]`
	xpEls, err := d.FindAllByXPath(xpath4)
	if err == nil {
		fmt.Printf("✅ 当前屏幕上共有 %d 个可点击的元素\n", len(xpEls))
		for i, node := range xpEls {
			// 只打印前 3 个以免刷屏
			if i < 3 {
				fmt.Printf("   [%d] Text: '%s', Bounds: '%s'\n", i, node.Text(), node.Attr("bounds"))
			}
		}
	}
}
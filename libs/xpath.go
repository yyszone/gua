package libs

import (
    "fmt"
    "strconv"
    "strings"
    "github.com/antchfx/xmlquery"
)

// FindByXPath 通过标准 XPath 语法查找元素
// 支持完整 XPath 1.0 语法，例如：
//   //android.widget.TextView[@text="支付宝"]
//   //node[@resource-id="com.miui.home:id/icon_title"]
//   //node[@text="支付宝" and @enabled="true"]
//   (//node[@clickable="true"])[2]
func (d *Device) FindByXPath(xpath string) (*XPathElement, error) {
    xml, err := d.DumpHierarchy(false, 50)
    if err != nil {
        return nil, err
    }

    doc, err := xmlquery.Parse(strings.NewReader(xml))
    if err != nil {
        return nil, fmt.Errorf("XML 解析失败: %w", err)
    }

    node := xmlquery.FindOne(doc, xpath)
    if node == nil {
        return nil, fmt.Errorf("XPath 未找到元素: %s", xpath)
    }

    return &XPathElement{device: d, node: node}, nil
}

// FindAllByXPath 查找所有匹配的元素
func (d *Device) FindAllByXPath(xpath string) ([]*XPathElement, error) {
    xml, err := d.DumpHierarchy(false, 50)
    if err != nil {
        return nil, err
    }

    doc, err := xmlquery.Parse(strings.NewReader(xml))
    if err != nil {
        return nil, fmt.Errorf("XML 解析失败: %w", err)
    }

    nodes := xmlquery.Find(doc, xpath)
    result := make([]*XPathElement, 0, len(nodes))
    for _, n := range nodes {
        result = append(result, &XPathElement{device: d, node: n})
    }
    return result, nil
}

// XPathElement XPath 找到的元素
type XPathElement struct {
    device *Device
    node   *xmlquery.Node
}

// Attr 获取元素属性
func (e *XPathElement) Attr(name string) string {
    return e.node.SelectAttr(name)
}

// Text 获取元素文本
func (e *XPathElement) Text() string {
    return e.Attr("text")
}

// ResourceId 获取资源 ID
func (e *XPathElement) ResourceId() string {
    return e.Attr("resource-id")
}

// Bounds 解析边界，返回 x, y, w, h
func (e *XPathElement) Bounds() (x, y, w, h int, err error) {
    // bounds 格式：[left,top][right,bottom]
    b := e.Attr("bounds")
    var left, top, right, bottom int
    _, err = fmt.Sscanf(b, "[%d,%d][%d,%d]", &left, &top, &right, &bottom)
    if err != nil {
        return 0, 0, 0, 0, fmt.Errorf("解析 bounds 失败: %s", b)
    }
    return left, top, right - left, bottom - top, nil
}

// Center 获取元素中心坐标
func (e *XPathElement) Center() (x, y int, err error) {
    left, top, w, h, err := e.Bounds()
    if err != nil {
        return 0, 0, err
    }
    return left + w/2, top + h/2, nil
}

// Click 点击元素
func (e *XPathElement) Click() error {
    x, y, err := e.Center()
    if err != nil {
        return err
    }
    return e.device.FastClick(x, y)
}

// Clickable 是否可点击
func (e *XPathElement) Clickable() bool {
    return e.Attr("clickable") == "true"
}

// Enabled 是否可用
func (e *XPathElement) Enabled() bool {
    return e.Attr("enabled") == "true"
}

// Index 获取索引
func (e *XPathElement) Index() int {
    v, _ := strconv.Atoi(e.Attr("index"))
    return v
}
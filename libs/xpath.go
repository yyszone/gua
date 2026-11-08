package libs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/antchfx/xmlquery"
)

// =========================================================================
// 🌟 1. 结构化层次树（单次 Dump 内存缓存）
// =========================================================================

// Hierarchy 缓存了单次 Dump 的 XML DOM 树
type Hierarchy struct {
	device *Device
	doc    *xmlquery.Node
}

// GetHierarchy 从手机 Dump 布局并解析为内存 DOM 树（供批量/多次内存查询）
func (d *Device) GetHierarchy() (*Hierarchy, error) {
	xmlStr, err := d.DumpHierarchy(false, 50)
	if err != nil {
		return nil, err
	}

	doc, err := xmlquery.Parse(strings.NewReader(xmlStr))
	if err != nil {
		return nil, fmt.Errorf("XML 解析失败: %w", err)
	}

	return &Hierarchy{device: d, doc: doc}, nil
}

// FindByXPath 在已有的 DOM 树中查找单个元素（0 网络开销）
func (h *Hierarchy) FindByXPath(xpath string) (*XPathElement, error) {
	node := xmlquery.FindOne(h.doc, xpath)
	if node == nil {
		return nil, fmt.Errorf("XPath 未找到元素: %s", xpath)
	}
	return &XPathElement{device: h.device, node: node}, nil
}

// FindAllByXPath 在已有的 DOM 树中查找所有匹配元素（0 网络开销）
func (h *Hierarchy) FindAllByXPath(xpath string) ([]*XPathElement, error) {
	nodes := xmlquery.Find(h.doc, xpath)
	result := make([]*XPathElement, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, &XPathElement{device: h.device, node: n})
	}
	return result, nil
}

// FindAnyByXPath 🌟【核心提速函数】：只 Dump 一次，在内存中依次查找传入的多个 XPath
// 一旦某个 XPath 命中，立即返回该元素及其在列表中的索引 index
func (d *Device) FindAnyByXPath(xpaths []string) (*XPathElement, int, error) {
	if len(xpaths) == 0 {
		return nil, -1, fmt.Errorf("xpaths 列表为空")
	}

	h, err := d.GetHierarchy()
	if err != nil {
		return nil, -1, err
	}

	for i, xp := range xpaths {
		if node := xmlquery.FindOne(h.doc, xp); node != nil {
			return &XPathElement{device: d, node: node}, i, nil
		}
	}

	return nil, -1, fmt.Errorf("所有 XPath 均未匹配")
}

// FindByXPath 保持原有兼容性（内部走 Hierarchy）
func (d *Device) FindByXPath(xpath string) (*XPathElement, error) {
	h, err := d.GetHierarchy()
	if err != nil {
		return nil, err
	}
	return h.FindByXPath(xpath)
}

// FindAllByXPath 保持原有兼容性
func (d *Device) FindAllByXPath(xpath string) ([]*XPathElement, error) {
	h, err := d.GetHierarchy()
	if err != nil {
		return nil, err
	}
	return h.FindAllByXPath(xpath)
}

// =========================================================================
// 🌟 2. XPathElement 增强（支持找子元素、兄弟节点、父节点）
// =========================================================================

type XPathElement struct {
	device *Device
	node   *xmlquery.Node
}

// Node 暴露底层 *xmlquery.Node 供高级自定义操作
func (e *XPathElement) Node() *xmlquery.Node {
	return e.node
}

// FindChild 🌟 在当前元素下方查找子元素
// 注意：支持传入 "node[@text='xxx']"、".//node[...]" 或 "//node[...]"（会自动规整为相对当前节点查找）
func (e *XPathElement) FindChild(xpath string) (*XPathElement, error) {
	relXPath := xpath
	if strings.HasPrefix(relXPath, "//") {
		// 标准 XPath 中，"//" 会跳回根节点。必须加上 "." 才是以当前节点为基准查找
		relXPath = "." + relXPath
	} else if !strings.HasPrefix(relXPath, "./") && !strings.HasPrefix(relXPath, ".//") {
		relXPath = ".//" + relXPath
	}

	childNode := xmlquery.FindOne(e.node, relXPath)
	if childNode == nil {
		return nil, fmt.Errorf("在当前节点下未找到子元素: %s (实际执行: %s)", xpath, relXPath)
	}

	return &XPathElement{device: e.device, node: childNode}, nil
}

// FindChildren 🌟 在当前元素下方查找所有符合条件的子元素列表
func (e *XPathElement) FindChildren(xpath string) ([]*XPathElement, error) {
	relXPath := xpath
	if strings.HasPrefix(relXPath, "//") {
		relXPath = "." + relXPath
	} else if !strings.HasPrefix(relXPath, "./") && !strings.HasPrefix(relXPath, ".//") {
		relXPath = ".//" + relXPath
	}

	nodes := xmlquery.Find(e.node, relXPath)
	result := make([]*XPathElement, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, &XPathElement{device: e.device, node: n})
	}
	return result, nil
}

// Parent 获取父级元素节点
func (e *XPathElement) Parent() *XPathElement {
	if e.node.Parent == nil {
		return nil
	}
	return &XPathElement{device: e.device, node: e.node.Parent}
}

// NextSibling 获取同级下一个兄弟节点
func (e *XPathElement) NextSibling() *XPathElement {
	if e.node.NextSibling == nil {
		return nil
	}
	return &XPathElement{device: e.device, node: e.node.NextSibling}
}

// PrevSibling 获取同级上一个兄弟节点
func (e *XPathElement) PrevSibling() *XPathElement {
	if e.node.PrevSibling == nil {
		return nil
	}
	return &XPathElement{device: e.device, node: e.node.PrevSibling}
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

// Info 获取所有属性
func (e *XPathElement) Info() map[string]interface{} {
	m := make(map[string]interface{})
	for _, attr := range e.node.Attr {
		key := attr.Name.Local
		val := attr.Value

		if val == "true" {
			m[key] = true
			continue
		}
		if val == "false" {
			m[key] = false
			continue
		}

		if i, err := strconv.Atoi(val); err == nil {
			m[key] = i
			continue
		}

		m[key] = val
	}
	return m
}

// ToJSON 返回格式化后的 JSON
func (e *XPathElement) ToJSON() string {
	data := e.Info()
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
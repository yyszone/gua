package libs

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"image/png"
	"math"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── OCR 全局互斥锁（防 CPU 峰值，同一时间只跑一个 tesseract） ────────────────
var ocrMutex sync.Mutex

// ─── 每台设备独立的截图 hash 缓存 ─────────────────────────────────────────────
// 缓存结构：{图片 hash + opts key} → OCRResult
// 当截图字节完全相同时直接返回上次结果，省掉整个 tesseract 调用。

type ocrCacheEntry struct {
	imgHash   uint64
	optsKey   string
	result    *OCRResult
	createdAt time.Time
}

// ocrCacheTTL 缓存最长有效时间。
// 超过这个时间即使截图 hash 相同也重新识别（防止 UI 变化但像素巧合一致）。
const ocrCacheTTL = 3 * time.Second

var (
	// 按设备 IP 存缓存，避免多设备互相污染
	deviceOCRCaches   = make(map[string]*ocrCacheEntry)
	deviceOCRCachesMu sync.Mutex
)

// hashImgData 用 FNV-64 计算截图字节 hash（极快，约 1GB/s，不需要密码学安全）
func hashImgData(data []byte) uint64 {
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

// optsKey 把 OCROptions 里影响识别结果的字段序列化成字符串，作为缓存 key 的一部分
func optsKey(o OCROptions) string {
	// 👉 【优化】：将 o.Config 纳入缓存特征计算，确保修改命令行参数时能立即刷新缓存
	return fmt.Sprintf("%s|%d|%s|%d|%d|%d|%v|%s|%s",
		o.Lang, o.PSM, o.SearchText,
		o.BinarizeNum, o.HorizontalGapThreshold, o.VerticalGapThreshold,
		o.XYPercentage, o.TextPadding, o.Config)
}

// ─── 公开类型定义（不变）──────────────────────────────────────────────────────

// OCROptions 定义 OCR 识别选项
type OCROptions struct {
	ImageBytes             []byte // 新增：如果传入了图片字节，则直接识别该图片，不再调用截图
	Lang                   string // 识别语言，默认 "chi_sim"
	PSM                    int    // 页面分割模式，默认 3
	Config                 string // 👉 【新增】：支持传入自定义参数，例如 "--psm 6" 或 "-c tessedit_char_whitelist=0-9"
	SearchText             string // 搜索过滤文本，为空则不过滤
	BinarizeNum            int    // 二值化阈值 (0-255)，<=0 表示不进行二值化，通常填 210
	HorizontalGapThreshold int    // 水平连通间隙阈值(像素)，默认 30
	VerticalGapThreshold   int    // 垂直行偏差阈值(像素)，默认 10
	XYPercentage           bool   // 是否返回百分比坐标 (0-1.0)，默认 false，返回绝对像素坐标
	TextPadding            string // 合并文本时的填充字符
}
// OCRRegion 识别出的每个连通文本区域信息
type OCRRegion struct {
	Text     string  `json:"text"`
	CenterX  float64 `json:"centerX"`
	CenterY  float64 `json:"centerY"`
	Accuracy float64 `json:"accuracy"`
	Box      [4]int  `json:"box"` // 左，上，右，下（绝对像素）
}

// OCRResult OCR 最终输出结果
type OCRResult struct {
	Regions []OCRRegion
	AllText string
}

// internalWord tesseract TSV 解析的内部结构
type internalWord struct {
	text   string
	left   int
	top    int
	width  int
	height int
	right  int
	bottom int
	conf   int
}

// ─── 主入口 ───────────────────────────────────────────────────────────────────

// OCR 对设备截图执行 OCR 识别。
// 若截图与上次完全一致（FNV hash 相同）且缓存未过期，直接返回缓存结果，
// 不调用 tesseract，节省 CPU 和时间。
// OCR 对设备截图执行 OCR 识别。
// 若截图与上次完全一致（FNV hash 相同）且缓存未过期，直接返回缓存结果，
// 不调用 tesseract，节省 CPU 和时间。
func (d *Device) OCR(opts OCROptions) (*OCRResult, error) {
	// 填默认值
	if opts.Lang == "" {
		opts.Lang = "chi_sim"
	}
	if opts.PSM == 0 {
		opts.PSM = 3
	}
	if opts.HorizontalGapThreshold <= 0 {
		opts.HorizontalGapThreshold = 30
	}
	if opts.VerticalGapThreshold <= 0 {
		opts.VerticalGapThreshold = 10
	}

	// ── 1. 获取图片（优先使用传入的 ImageBytes） ──
	var imgData []byte
	var err error
	if len(opts.ImageBytes) > 0 {
		// 传入了图片，直接用
		imgData = opts.ImageBytes
	} else {
		// 没有传入图片，走全屏截图
		imgData, err = d.Screenshot()
		if err != nil {
			return nil, fmt.Errorf("获取截图失败: %w", err)
		}
	}

	// ── 2. 计算 hash，命中缓存则直接返回 ──
	imgHash := hashImgData(imgData)
	ok := optsKey(opts)
    
	deviceOCRCachesMu.Lock()
	entry := deviceOCRCaches[d.serial]
	deviceOCRCachesMu.Unlock()

	if entry != nil &&
		entry.imgHash == imgHash &&
		entry.optsKey == ok &&
		time.Since(entry.createdAt) < ocrCacheTTL {
		// 缓存命中：截图未变化，直接返回上次结果
		return entry.result, nil
	}

	// ── 3. 二值化（可选） ──
	processedData := imgData
	if opts.BinarizeNum > 0 && opts.BinarizeNum <= 255 {
		processedData, err = binarizeImage(imgData, uint8(opts.BinarizeNum))
		if err != nil {
			return nil, fmt.Errorf("图片二值化失败: %w", err)
		}
	}

	// ── 4. 解码一次，同时取宽高 ──
	srcImg, _, err := image.Decode(bytes.NewReader(processedData))
	if err != nil {
		return nil, fmt.Errorf("解码图片失败: %w", err)
	}
	imgW := float64(srcImg.Bounds().Dx())
	imgH := float64(srcImg.Bounds().Dy())

	// 把解码后的图片重新编为 PNG 供 tesseract 读取
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, srcImg); err != nil {
		return nil, fmt.Errorf("PNG 重编码失败: %w", err)
	}
	srcImg = nil // 允许 GC 回收解码后的像素内存

	// ── 5. 动态装配 tesseract 命令参数 ──
	ocrMutex.Lock()
	defer ocrMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 默认基础参数
	args := []string{"stdin", "stdout", "-l", opts.Lang}

	// 解析可能传入的自定义配置字符串 (如 "--psm 6" 或 "-c tessedit_char_whitelist=0-9")
	var configArgs []string
	if opts.Config != "" {
		configArgs = strings.Fields(opts.Config)
	}

	// 安全保护：检测自定义配置中是否已包含 --psm 参数
	hasCustomPSM := false
	for _, arg := range configArgs {
		if arg == "--psm" {
			hasCustomPSM = true
			break
		}
	}

	// 如果自定义配置里没有指定 --psm，才追加默认的 opts.PSM，防止参数冲突导致运行报错
	if !hasCustomPSM {
		args = append(args, "--psm", strconv.Itoa(opts.PSM))
	}

	// 追加自定义参数
	if len(configArgs) > 0 {
		args = append(args, configArgs...)
	}

	// 指定输出格式为 tsv
	args = append(args, "tsv")

	// 管道传图运行
	cmd := exec.CommandContext(ctx, "tesseract", args...)
	cmd.Stdin = &pngBuf

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf

	if err := cmd.Run(); err != nil {
		if _, lookErr := exec.LookPath("tesseract"); lookErr != nil {
			return nil, fmt.Errorf("未检测到 tesseract：\n%s\n原始错误: %v",
				tesseractInstallHint(), err)
		}
		return nil, fmt.Errorf("tesseract 执行失败（参数错误或缺少语言包）: %w", err)
	}

	// ── 6. 解析 TSV ──
	result, err := parseTesseractTSV(outBuf.Bytes(), imgW, imgH, opts)
	if err != nil {
		return nil, err
	}

	// ── 7. 存入缓存 ──
	deviceOCRCachesMu.Lock()
	deviceOCRCaches[d.serial] = &ocrCacheEntry{
		imgHash:   imgHash,
		optsKey:   ok,
		result:    result,
		createdAt: time.Now(),
	}
	deviceOCRCachesMu.Unlock()

	return result, nil
}


// InvalidateOCRCache 主动清除指定设备的 OCR 缓存（点击/操作后如需立即重识别，调用此函数）
func (d *Device) InvalidateOCRCache() {
	deviceOCRCachesMu.Lock()
	delete(deviceOCRCaches, d.serial)
	deviceOCRCachesMu.Unlock()
}

// ─── 二值化（优化版：直接访问像素，避免 color interface 调用） ────────────────

// binarizeImage 对图片进行灰度二值化。
// 优化：对 *image.NRGBA / *image.RGBA / *image.Gray 类型直接读取底层字节，
//
//	避免每像素的 interface 调用（原版用 color.GrayModel.Convert，约慢 3-5 倍）。
func binarizeImage(imgData []byte, threshold uint8) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewGray(b)
	dstPix := dst.Pix

	switch img := src.(type) {
	case *image.NRGBA:
		// 最常见类型（PNG 解码结果）：NRGBA，每像素 4 字节
		for y := 0; y < h; y++ {
			srcRow := img.Pix[y*img.Stride : y*img.Stride+w*4]
			dstRow := dstPix[y*w : y*w+w]
			for x := 0; x < w; x++ {
				r := uint32(srcRow[x*4])
				g := uint32(srcRow[x*4+1])
				bl := uint32(srcRow[x*4+2])
				// ITU-R BT.601：0.299R + 0.587G + 0.114B
				gray := uint8((r*19595 + g*38470 + bl*7471 + 1<<15) >> 16)
				if gray > threshold {
					dstRow[x] = 255
				} else {
					dstRow[x] = 0
				}
			}
		}
	case *image.RGBA:
		for y := 0; y < h; y++ {
			srcRow := img.Pix[y*img.Stride : y*img.Stride+w*4]
			dstRow := dstPix[y*w : y*w+w]
			for x := 0; x < w; x++ {
				r := uint32(srcRow[x*4])
				g := uint32(srcRow[x*4+1])
				bl := uint32(srcRow[x*4+2])
				gray := uint8((r*19595 + g*38470 + bl*7471 + 1<<15) >> 16)
				if gray > threshold {
					dstRow[x] = 255
				} else {
					dstRow[x] = 0
				}
			}
		}
	case *image.Gray:
		// 已经是灰度图，直接比阈值
		for i, v := range img.Pix {
			if i >= len(dstPix) {
				break
			}
			if v > threshold {
				dstPix[i] = 255
			} else {
				dstPix[i] = 0
			}
		}
	default:
		// 通用 fallback（其他格式如 YCbCr）
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.GrayModel.Convert(src.At(x, y)).(color.Gray)
				if c.Y > threshold {
					dst.SetGray(x, y, color.Gray{Y: 255})
				} else {
					dst.SetGray(x, y, color.Gray{Y: 0})
				}
			}
		}
	}

	var buf bytes.Buffer
	buf.Grow(w * h / 2) // 预分配，灰度 PNG 压缩后约为原大小一半
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BinarizeImage 公开的二值化接口（保持向后兼容）
func BinarizeImage(imgData []byte, threshold uint8) ([]byte, error) {
	return binarizeImage(imgData, threshold)
}

// ─── TSV 解析（逻辑不变，仅优化内存分配） ────────────────────────────────────

func parseTesseractTSV(tsvData []byte, imgW, imgH float64, opts OCROptions) (*OCRResult, error) {
	reader := csv.NewReader(bytes.NewReader(tsvData))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	// 预分配（通常一屏 20-60 个词）
	words := make([]internalWord, 0, 64)

	for i, row := range records {
		if i == 0 || len(row) < 12 {
			continue
		}
		text := strings.TrimSpace(row[11])
		if text == "" {
			continue
		}
		if opts.SearchText != "" && !strings.Contains(text, opts.SearchText) {
			continue
		}
		conf, _ := strconv.Atoi(row[10])
		if conf < 0 {
			continue
		}
		left, _ := strconv.Atoi(row[6])
		top, _ := strconv.Atoi(row[7])
		width, _ := strconv.Atoi(row[8])
		height, _ := strconv.Atoi(row[9])

		words = append(words, internalWord{
			text:   text,
			left:   left,
			top:    top,
			width:  width,
			height: height,
			right:  left + width,
			bottom: top + height,
			conf:   conf,
		})
	}

	if len(words) == 0 {
		return &OCRResult{}, nil
	}

	// ── 按 top 排序后分行 ──
	sort.Slice(words, func(i, j int) bool { return words[i].top < words[j].top })

	lines := make([][]internalWord, 0, 16)
	for _, w := range words {
		if len(lines) == 0 {
			lines = append(lines, []internalWord{w})
			continue
		}
		last := lines[len(lines)-1]
		// 计算当前行平均 top（用整数运算）
		sumTop := 0
		for _, cw := range last {
			sumTop += cw.top
		}
		avgTop := sumTop / len(last)
		diff := w.top - avgTop
		if diff < 0 {
			diff = -diff
		}
		if diff <= opts.VerticalGapThreshold {
			lines[len(lines)-1] = append(lines[len(lines)-1], w)
		} else {
			lines = append(lines, []internalWord{w})
		}
	}

	// ── 每行内水平分块 ──
	groups := make([][]internalWord, 0, len(words))
	for _, line := range lines {
		sort.Slice(line, func(i, j int) bool { return line[i].left < line[j].left })
		cur := []internalWord{line[0]}
		for i := 1; i < len(line); i++ {
			if line[i].left-cur[len(cur)-1].right <= opts.HorizontalGapThreshold {
				cur = append(cur, line[i])
			} else {
				groups = append(groups, cur)
				cur = []internalWord{line[i]}
			}
		}
		groups = append(groups, cur)
	}

	// ── 组装 OCRResult ──
	result := &OCRResult{
		Regions: make([]OCRRegion, 0, len(groups)),
	}
	var allText strings.Builder
	allText.Grow(len(groups) * 8)

	replacer := strings.NewReplacer(" ", "") // 复用，避免每次重新构造

	for _, group := range groups {
		var textBuf strings.Builder
		var sumConf int
		minX, minY := math.MaxInt32, math.MaxInt32
		maxX, maxY := 0, 0

		for _, w := range group {
			textBuf.WriteString(w.text)
			sumConf += w.conf
			if w.left < minX {
				minX = w.left
			}
			if w.top < minY {
				minY = w.top
			}
			if w.right > maxX {
				maxX = w.right
			}
			if w.bottom > maxY {
				maxY = w.bottom
			}
		}

		groupText := replacer.Replace(textBuf.String())
		avgConf := float64(sumConf) / float64(len(group))
		cx := float64(minX + (maxX-minX)/2)
		cy := float64(minY + (maxY-minY)/2)

		if opts.XYPercentage && imgW > 0 && imgH > 0 {
			cx = math.Round((cx/imgW)*100) / 100
			cy = math.Round((cy/imgH)*100) / 100
		}

		result.Regions = append(result.Regions, OCRRegion{
			Text:     groupText,
			CenterX:  cx,
			CenterY:  cy,
			Accuracy: avgConf,
			Box:      [4]int{minX, minY, maxX, maxY},
		})
		allText.WriteString(groupText)
		allText.WriteString(opts.TextPadding)
	}

	result.AllText = allText.String()
	return result, nil
}

// ─── tesseract 未安装提示 ────────────────────────────────────────────────────

func tesseractInstallHint() string {
	switch runtime.GOOS {
	case "linux":
		return `Debian/Ubuntu:
  sudo apt install -y tesseract-ocr tesseract-ocr-chi-sim
Fedora/CentOS:
  sudo dnf install -y tesseract
验证: tesseract --version && tesseract --list-langs`
	case "darwin":
		return `brew install tesseract
chi_sim.traineddata 放到 $(brew --prefix)/share/tessdata`
	case "windows":
		return `choco install -y tesseract
或从 UB Mannheim 下载安装包，安装目录加入 PATH`
	default:
		return "请根据平台安装 tesseract 并确保在 PATH 中"
	}
}


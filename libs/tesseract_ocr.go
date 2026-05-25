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

type ocrCacheEntry struct {
	imgHash   uint64
	optsKey   string
	result    *OCRResult
	createdAt time.Time
}

const ocrCacheTTL = 3 * time.Second

var (
	// 按设备 IP 存缓存，避免多设备互相污染
	deviceOCRCaches   = make(map[string]*ocrCacheEntry)
	// 👉 升级为读写锁，支持多设备高并发时，无锁竞争地安全并行读取缓存
	deviceOCRCachesMu sync.RWMutex

	// 👉 【核心优化：依赖注入挂钩】
	// 允许外部（如 upper-level 调度器）注入分布式锁。
	// 若外部注入了该函数，本库的所有 OCR 进程都会自动通过该挂钩获取全局锁；
	// 若未注入（或独立运行本库），则自动安全退回到本地单进程 ocrMutex，100% 避免循环导入。
	DistLockHook func(serial string, timeout time.Duration) (unlock func(), err error)
)

// hashImgData 用 FNV-64 计算截图字节 hash
func hashImgData(data []byte) uint64 {
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

// optsKey 缓存 key
func optsKey(o OCROptions) string {
	return fmt.Sprintf("%s|%d|%s|%d|%d|%d|%v|%s|%s",
		o.Lang, o.PSM, o.SearchText,
		o.BinarizeNum, o.HorizontalGapThreshold, o.VerticalGapThreshold,
		o.XYPercentage, o.TextPadding, o.Config)
}

// ─── 公开类型定义 ──────────────────────────────────────────────────────

type OCROptions struct {
	ImageBytes             []byte 
	Lang                   string 
	PSM                    int    
	Config                 string 
	SearchText             string 
	BinarizeNum            int    
	HorizontalGapThreshold int    
	VerticalGapThreshold   int    
	XYPercentage           bool   
	TextPadding            string 
}

type OCRRegion struct {
	Text     string  `json:"text"`
	CenterX  float64 `json:"centerX"`
	CenterY  float64 `json:"centerY"`
	Accuracy float64 `json:"accuracy"`
	Box      [4]int  `json:"box"` 
}

type OCRResult struct {
	Regions []OCRRegion
	AllText string
}

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
		imgData = opts.ImageBytes
	} else {
		imgData, err = d.Screenshot()
		if err != nil {
			return nil, fmt.Errorf("获取截图失败: %w", err)
		}
	}

	// ── 2. 计算 hash，命中缓存则直接返回 ──
	imgHash := hashImgData(imgData)
	ok := optsKey(opts)
    
	deviceOCRCachesMu.RLock()
	entry := deviceOCRCaches[d.serial]
	deviceOCRCachesMu.RUnlock()

	if entry != nil &&
		entry.imgHash == imgHash &&
		entry.optsKey == ok &&
		time.Since(entry.createdAt) < ocrCacheTTL {
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

	// ── 4. 轻量级尺寸解析 ──
	cfg, _, err := image.DecodeConfig(bytes.NewReader(processedData))
	if err != nil {
		return nil, fmt.Errorf("解析图片尺寸失败: %w", err)
	}
	imgW := float64(cfg.Width)
	imgH := float64(cfg.Height)

	// ── 5. 👉【核心优化】：调用全局锁/本地锁 ──
	var unlock func()
	if DistLockHook != nil {
		var lockErr error
		// 尝试获取外部注入的全局唯一锁，使用 d.serial 代替未定义的 d.IP（d.serial 为底层 Device 导出字段）
		unlock, lockErr = DistLockHook(d.serial, 30*time.Second)
		if lockErr != nil {
			return nil, fmt.Errorf("等待全局 OCR 锁超时: %w", lockErr)
		}
	} else {
		// 降级保护：回退到本地单进程互斥锁
		ocrMutex.Lock()
		unlock = ocrMutex.Unlock
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 默认基础参数
	args := []string{"stdin", "stdout", "-l", opts.Lang}

	var configArgs []string
	if opts.Config != "" {
		configArgs = strings.Fields(opts.Config)
	}

	hasCustomPSM := false
	for _, arg := range configArgs {
		if arg == "--psm" {
			hasCustomPSM = true
			break
		}
	}

	if !hasCustomPSM {
		args = append(args, "--psm", strconv.Itoa(opts.PSM))
	}

	if len(configArgs) > 0 {
		args = append(args, configArgs...)
	}

	args = append(args, "tsv")

	cmd := exec.CommandContext(ctx, "tesseract", args...)
	cmd.Stdin = bytes.NewReader(processedData)

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

// InvalidateOCRCache 清除缓存
func (d *Device) InvalidateOCRCache() {
	deviceOCRCachesMu.Lock()
	delete(deviceOCRCaches, d.serial)
	deviceOCRCachesMu.Unlock()
}

// ─── 二值化保持不变 ────────────────────────────────────

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
		for y := 0; y < h; y++ {
			srcRow := img.Pix[y*img.Stride : y*img.Stride+w*4]
			dstRow := dstPix[y*w : y*w+w]
			for x := 0; x < w; x++ {
				r := uint32(srcRow[x*4])
				g := uint32(srcRow[x*4+1])
				bl := uint32(srcRow[x*4+2])
				gray := uint8((r*19595 + g*38400 + bl*7471 + 1<<15) >> 16)
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
				gray := uint8((r*19595 + g*38400 + bl*7471 + 1<<15) >> 16)
				if gray > threshold {
					dstRow[x] = 255
				} else {
					dstRow[x] = 0
				}
			}
		}
	case *image.Gray:
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
	buf.Grow(w * h / 2) 
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func BinarizeImage(imgData []byte, threshold uint8) ([]byte, error) {
	return binarizeImage(imgData, threshold)
}

// ─── TSV 解析保持不变 ────────────────────────────────────

func parseTesseractTSV(tsvData []byte, imgW, imgH float64, opts OCROptions) (*OCRResult, error) {
	reader := csv.NewReader(bytes.NewReader(tsvData))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

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

	sort.Slice(words, func(i, j int) bool { return words[i].top < words[j].top })

	lines := make([][]internalWord, 0, 16)
	for _, w := range words {
		if len(lines) == 0 {
			lines = append(lines, []internalWord{w})
			continue
		}
		last := lines[len(lines)-1]
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

	result := &OCRResult{
		Regions: make([]OCRRegion, 0, len(groups)),
	}
	var allText strings.Builder
	allText.Grow(len(groups) * 8)

	replacer := strings.NewReplacer(" ", "") 

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
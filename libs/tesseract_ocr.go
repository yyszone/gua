package libs

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"image"
	_ "image/jpeg" // 必须：注册 JPEG 解码器
	"image/color"
	_ "image/png"  // 必须：注册 PNG 解码器
	"image/png"    // 用于 png.Encode
	"runtime"

	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ocrMutex 用于限制并发调用 OCR，防止 CPU 过载（模拟 Python 中的 FileLock）
var ocrMutex sync.Mutex

// OCROptions 定义 OCR 识别选项
type OCROptions struct {
	Lang                   string // 识别语言，默认 "chi_sim"
	PSM                    int    // 页面分割模式，默认 3
	SearchText             string // 搜索过滤文本，为空则不过滤
	BinarizeNum            int    // 二值化阈值 (0-255)，<=0 表示不进行二值化，通常填 210
	HorizontalGapThreshold int    // 水平连通间隙阈值(像素)，默认 30
	VerticalGapThreshold   int    // 垂直行偏差阈值(像素)，默认 10
	XYPercentage           bool   // 是否返回百分比坐标 (0-1.0)，默认 false，返回绝对像素坐标
	TextPadding            string // 合并文本时的填充字符
}

// OCRRegion 识别出的每个连通文本区域信息
type OCRRegion struct {
	Text     string  `json:"text"`     // 合并后的文本
	CenterX  float64 `json:"centerX"`  // 中心点 X（百分比或像素）
	CenterY  float64 `json:"centerY"`  // 中心点 Y（百分比或像素）
	Accuracy float64 `json:"accuracy"` // 区域内平均置信度
	Box      [4]int  `json:"box"`      // 左，上，右，下 (绝对像素值)
}

// OCRResult OCR 的最终输出结果
type OCRResult struct {
	Regions []OCRRegion
	AllText string // 全部合并的文本
}

// internalWord 解析 tesseract tsv 后的内部单字/词结构
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


func tesseractInstallHint() string {
    switch runtime.GOOS {
    case "linux":
        return `常见安装方法（请选择与你的发行版对应的命令）：
- Debian / Ubuntu:
  sudo apt update
  sudo apt install -y tesseract-ocr tesseract-ocr-chi-sim
- Fedora / CentOS (dnf):
  sudo dnf install -y tesseract
安装后请确认 chi_sim 语言包在 tessdata 目录（如 /usr/share/tessdata 或 /usr/local/share/tessdata）。
验证：tesseract --version && tesseract --list-langs`
    case "darwin":
        return `macOS 安装示例（需要 Homebrew）：
  brew install tesseract
若需中文语言包，请确保 chi_sim.traineddata 放到 $(brew --prefix)/share/tessdata
验证：tesseract --version && tesseract --list-langs`
    case "windows":
        return `Windows 安装示例：
- 使用 Chocolatey:
  choco install -y tesseract
- 或从 UB Mannheim 等站点下载 Windows 安装包并安装（安装时记下安装目录并加入 PATH）
验证：在 PowerShell/CMD 运行 tesseract --version && tesseract --list-langs`
    default:
        return "未检测到 tesseract 可执行文件，请根据你的平台安装 tesseract 并确保可执行文件在 PATH 中。"
    }
}

// BinarizeImage 进行图片二值化，使用标准库处理
func BinarizeImage(imgData []byte, threshold uint8) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	binImg := image.NewGray(bounds)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			// 转为灰度并应用阈值
			grayColor := color.GrayModel.Convert(img.At(x, y)).(color.Gray)
			if grayColor.Y > threshold {
				binImg.SetGray(x, y, color.Gray{Y: 255})
			} else {
				binImg.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, binImg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OCR 对当前设备的屏幕截图执行 OCR 识别
func (d *Device) OCR(opts OCROptions) (*OCRResult, error) {
	// 设置默认值
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

	// 1. 获取截图
	imgData, err := d.Screenshot()
	if err != nil {
		return nil, fmt.Errorf("获取截图失败: %w", err)
	}

	// 解析出图片的宽高，用于算百分比
	imgConfig, _, err := image.DecodeConfig(bytes.NewReader(imgData))
	if err != nil {
		return nil, fmt.Errorf("解析图片尺寸失败: %w", err)
	}
	imgW, imgH := float64(imgConfig.Width), float64(imgConfig.Height)

	// 2. 如果配置了二值化，则先进行二值化处理
	if opts.BinarizeNum > 0 && opts.BinarizeNum <= 255 {
		imgData, err = BinarizeImage(imgData, uint8(opts.BinarizeNum))
		if err != nil {
			return nil, fmt.Errorf("图片二值化失败: %w", err)
		}
	}

	// 加锁执行 tesseract (防 CPU 峰值)
	ocrMutex.Lock()
	defer ocrMutex.Unlock()

	// 3. 将图片保存到临时文件供 tesseract 使用
	tmpFile, err := os.CreateTemp("", "ocr_target_*.png")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name()) // 执行完毕后删除临时文件
	if _, err := tmpFile.Write(imgData); err != nil {
		return nil, err
	}
	tmpFile.Close()

	// 4. 调用 tesseract 执行 OCR 并输出 TSV
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tesseract", tmpFile.Name(), "stdout", "-l", opts.Lang, "--psm", strconv.Itoa(opts.PSM), "tsv")

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	if err := cmd.Run(); err != nil {
		// 仅在出错时再判断是否为未安装 tesseract 导致
		if _, lookErr := exec.LookPath("tesseract"); lookErr != nil {
			// 未安装或不可用，返回带平台安装提示的友好错误
			hint := tesseractInstallHint()
			return nil, fmt.Errorf("执行 Tesseract 失败：未检测到 tesseract 可执行文件。\n%s\n原始错误: %v", hint, err)
		}
		// tesseract 存在但执行失败（例如超时、权限、语言包缺失等），返回原始错误并附加简短提示
		return nil, fmt.Errorf("执行 Tesseract 失败（请检查 tesseract 可用性与语言包）：%w", err)
	}

	// 5. 解析 TSV 输出
	return parseTesseractTSV(outBuf.Bytes(), imgW, imgH, opts)
}

func parseTesseractTSV(tsvData []byte, imgW, imgH float64, opts OCROptions) (*OCRResult, error) {
	reader := csv.NewReader(bytes.NewReader(tsvData))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var words []internalWord

	// 提取有效单词 (TSV 第一行是表头)
	for i, row := range records {
		if i == 0 || len(row) < 12 {
			continue // 表头或不完整行
		}
		text := strings.TrimSpace(row[11])
		if text == "" {
			continue
		}
		// 过滤 SearchText
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

	// --- 开始进行坐标聚类 ---

	// 1. 按垂直坐标（top）分行
	sort.Slice(words, func(i, j int) bool {
		return words[i].top < words[j].top
	})

	var lines [][]internalWord
	for _, w := range words {
		if len(lines) == 0 {
			lines = append(lines, []internalWord{w})
		} else {
			currLine := lines[len(lines)-1]
			sumTop := 0
			for _, cw := range currLine {
				sumTop += cw.top
			}
			avgTop := sumTop / len(currLine)
			// 如果 top 偏差在阈值内，算同一行
			if int(math.Abs(float64(w.top-avgTop))) <= opts.VerticalGapThreshold {
				lines[len(lines)-1] = append(lines[len(lines)-1], w)
			} else {
				// 新开一行
				lines = append(lines, []internalWord{w})
			}
		}
	}

	// 2. 对每行进行水平间隙分块
	var groups [][]internalWord
	for _, line := range lines {
		sort.Slice(line, func(i, j int) bool {
			return line[i].left < line[j].left
		})
		currGroup := []internalWord{line[0]}
		for i := 1; i < len(line); i++ {
			w := line[i]
			prev := currGroup[len(currGroup)-1]
			gap := w.left - prev.right
			if gap <= opts.HorizontalGapThreshold {
				currGroup = append(currGroup, w) // 算作连通，加入当前组
			} else {
				groups = append(groups, currGroup)
				currGroup = []internalWord{w} // 新开一组
			}
		}
		groups = append(groups, currGroup)
	}

	// 3. 计算各个区域的整体边界框、中心点、文本合并
	var result OCRResult
	var allTextBuilder strings.Builder

	for _, group := range groups {
		var groupText string
		var sumConf, minX, minY, maxX, maxY int
		minX, minY = math.MaxInt32, math.MaxInt32

		for _, w := range group {
			groupText += w.text
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

		groupText = strings.ReplaceAll(groupText, " ", "")
		avgConf := float64(sumConf) / float64(len(group))

		centerX := float64(minX + (maxX-minX)/2)
		centerY := float64(minY + (maxY-minY)/2)

		// 坐标百分比化
		if opts.XYPercentage && imgW > 0 && imgH > 0 {
			centerX = math.Round((centerX/imgW)*100) / 100
			centerY = math.Round((centerY/imgH)*100) / 100
		}

		region := OCRRegion{
			Text:     groupText,
			CenterX:  centerX,
			CenterY:  centerY,
			Accuracy: avgConf,
			Box:      [4]int{minX, minY, maxX, maxY},
		}
		result.Regions = append(result.Regions, region)

		allTextBuilder.WriteString(groupText)
		allTextBuilder.WriteString(opts.TextPadding)
	}

	result.AllText = allTextBuilder.String()
	return &result, nil
}
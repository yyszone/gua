package libs

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	imageMatchCoarseStep = 4
	imageMatchSampleStep = 4
	imageMatchFineRadius = imageMatchCoarseStep + 1

	imageMatchFlatVarianceThreshold = 120.0
	imageMatchMaxFlatPenalty        = 0.5
)

var (
	// ErrNoImageData 表示传入的图像数据为空。
	ErrNoImageData = errors.New("图像数据为空")
	// ErrInvalidImage 表示图像无法解码或图像尺寸无效。
	ErrInvalidImage = errors.New("无效图像")
	// ErrInvalidMatchThreshold 表示相似度阈值不在 [0, 1] 范围内。
	ErrInvalidMatchThreshold = errors.New("相似度阈值必须在 0 到 1 之间")
	// ErrTemplateLarger 表示模板图比屏幕截图大。
	ErrTemplateLarger = errors.New("模板比截图大")
	// ErrTransparentTemplate 表示模板没有可参与匹配的不透明采样像素。
	ErrTransparentTemplate = errors.New("模板没有可匹配的非透明像素")
	// ErrImageNoMatch 表示没有找到达到阈值的匹配位置。
	ErrImageNoMatch = errors.New("没有找到达到阈值的匹配位置")
)

// ImageMatchOptions 配置图像匹配行为。
//
// Threshold 是匹配阈值，取值范围为 [0, 1]。例如 0.85 表示要求至少 85% 的匹配分数。
// Workers 只影响 MatchAll 的粗扫描并发数；0 表示根据 CPU 数量自动选择。
// Match 使用单线程扫描，适合由调用方并发匹配多个模板。
type ImageMatchOptions struct {
	Threshold float64
	Workers   int
}

// ImageMatchResult 表示图像匹配结果。
// X、Y 是匹配区域的中心点坐标，单位为像素；Score 为最终匹配分数，范围通常为 [0, 1]。
type ImageMatchResult struct {
	X     int
	Y     int
	Score float64
}

// ImageFrame 是一次已解码的屏幕截图。
// 创建后内部像素数据只读，可以安全地被多个 goroutine 并发匹配。
type ImageFrame struct {
	rgb    []uint8
	width  int
	height int
}

// ImageTemplate 是一张已解码并预处理的模板图片。
// 创建后内部数据只读，可以重复用于多个 ImageFrame。
type ImageTemplate struct {
	rgbaData  []uint8
	width     int
	height    int
	sampCount int64
	maxSAD    int64
	hasAlpha  bool
}

type imageMatchRawHit struct {
	x, y  int
	score float64
}

// NewImageFrame 解码截图数据并转换为 RGB 像素数组。
// 支持 Go 标准库注册的 PNG、JPEG 和 GIF 格式。
func NewImageFrame(data []byte) (*ImageFrame, error) {
	if len(data) == 0 {
		return nil, ErrNoImageData
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w：解码截图失败：%v", ErrInvalidImage, err)
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w：截图尺寸无效 %dx%d", ErrInvalidImage, width, height)
	}
	if int64(width)*int64(height) > int64(imageMatchMaxIntOf()/3) {
		return nil, fmt.Errorf("%w：截图尺寸过大 %dx%d", ErrInvalidImage, width, height)
	}

	rgb := make([]uint8, width*height*3)
	imageMatchFillRGB(img, rgb)

	return &ImageFrame{
		rgb:    rgb,
		width:  width,
		height: height,
	}, nil
}

// NewImageTemplate 解码模板图片，并预计算 RGBA 数据、透明像素信息和 SAD 归一化参数。
func NewImageTemplate(data []byte) (*ImageTemplate, error) {
	if len(data) == 0 {
		return nil, ErrNoImageData
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w：解码模板失败：%v", ErrInvalidImage, err)
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w：模板尺寸无效 %dx%d", ErrInvalidImage, width, height)
	}
	if int64(width)*int64(height) > int64(imageMatchMaxIntOf()/4) {
		return nil, fmt.Errorf("%w：模板尺寸过大 %dx%d", ErrInvalidImage, width, height)
	}

	rgba := make([]uint8, width*height*4)
	hasAlpha := imageMatchFillRGBA(img, rgba)

	var sampleCount int64
	for y := 0; y < height; y += imageMatchSampleStep {
		for x := 0; x < width; x += imageMatchSampleStep {
			index := (y*width + x) * 4
			if !hasAlpha || rgba[index+3] >= 128 {
				sampleCount++
			}
		}
	}
	if sampleCount == 0 {
		return nil, ErrTransparentTemplate
	}
	if sampleCount > math.MaxInt64/(255*3) {
		return nil, fmt.Errorf("%w：模板采样点过多", ErrInvalidImage)
	}

	return &ImageTemplate{
		rgbaData:  rgba,
		width:     width,
		height:    height,
		sampCount: sampleCount,
		maxSAD:    sampleCount * 255 * 3,
		hasAlpha:  hasAlpha,
	}, nil
}

// Size 返回截图尺寸。
func (f *ImageFrame) Size() (width, height int) {
	if f == nil {
		return 0, 0
	}
	return f.width, f.height
}

// Size 返回模板尺寸。
func (t *ImageTemplate) Size() (width, height int) {
	if t == nil {
		return 0, 0
	}
	return t.width, t.height
}

// Match 在截图中寻找模板的最佳匹配位置。
// 返回坐标是匹配区域的中心点；未找到时返回 ErrImageNoMatch。
func (f *ImageFrame) Match(t *ImageTemplate, options ImageMatchOptions) (ImageMatchResult, error) {
	if f == nil || len(f.rgb) == 0 || f.width <= 0 || f.height <= 0 {
		return ImageMatchResult{}, fmt.Errorf("%w：截图对象无效", ErrInvalidImage)
	}
	if t == nil || len(t.rgbaData) == 0 || t.width <= 0 || t.height <= 0 || t.maxSAD <= 0 {
		return ImageMatchResult{}, fmt.Errorf("%w：模板对象无效", ErrInvalidImage)
	}
	if err := imageMatchValidateOptions(options); err != nil {
		return ImageMatchResult{}, err
	}
	if f.width < t.width || f.height < t.height {
		return ImageMatchResult{}, ErrTemplateLarger
	}

	searchW := f.width - t.width
	searchH := f.height - t.height
	maxSAD := float64(t.maxSAD)

	bestDiff := int64(math.MaxInt64)
	bestCX, bestCY := 0, 0
	for y := 0; y <= searchH; y += imageMatchCoarseStep {
		for x := 0; x <= searchW; x += imageMatchCoarseStep {
			diff := imageMatchSADRGB(f.rgb, f.width, x, y, t, imageMatchSampleStep, bestDiff)
			if diff < bestDiff {
				bestDiff = diff
				bestCX, bestCY = x, y
			}
		}
	}

	coarseScore := 1.0 - float64(bestDiff)/maxSAD
	if coarseScore < options.Threshold-0.15 {
		return ImageMatchResult{Score: coarseScore}, ErrImageNoMatch
	}

	earlyExitFine := int64(maxSAD * (1.0 - options.Threshold + 0.02))
	bestScore := coarseScore
	bestX, bestY := bestCX, bestCY

	y0 := imageMatchClamp(bestCY-imageMatchFineRadius, 0, searchH)
	y1 := imageMatchClamp(bestCY+imageMatchFineRadius, 0, searchH)
	x0 := imageMatchClamp(bestCX-imageMatchFineRadius, 0, searchW)
	x1 := imageMatchClamp(bestCX+imageMatchFineRadius, 0, searchW)

	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			diff := imageMatchSADRGB(f.rgb, f.width, x, y, t, imageMatchSampleStep, earlyExitFine)
			score := 1.0 - float64(diff)/maxSAD
			if score > bestScore {
				bestScore = score
				bestX, bestY = x, y
			}
		}
	}

	bestScore = imageMatchPenalizeFlatArea(f.rgb, f.width, bestX, bestY, t, bestScore)
	if bestScore < options.Threshold {
		return ImageMatchResult{Score: bestScore}, ErrImageNoMatch
	}

	return ImageMatchResult{
		X:     bestX + t.width/2,
		Y:     bestY + t.height/2,
		Score: bestScore,
	}, nil
}

// MatchAll 在截图中查找模板的所有匹配位置。
// 返回结果按 Score 从高到低排序；相邻候选点会通过聚类和 NMS 去重。
// 没有匹配时返回空切片和 nil。
func (f *ImageFrame) MatchAll(t *ImageTemplate, options ImageMatchOptions) ([]ImageMatchResult, error) {
	if f == nil || len(f.rgb) == 0 || f.width <= 0 || f.height <= 0 {
		return nil, fmt.Errorf("%w：截图对象无效", ErrInvalidImage)
	}
	if t == nil || len(t.rgbaData) == 0 || t.width <= 0 || t.height <= 0 || t.maxSAD <= 0 {
		return nil, fmt.Errorf("%w：模板对象无效", ErrInvalidImage)
	}
	if err := imageMatchValidateOptions(options); err != nil {
		return nil, err
	}
	if f.width < t.width || f.height < t.height {
		return nil, ErrTemplateLarger
	}

	searchW := f.width - t.width
	searchH := f.height - t.height
	maxSAD := float64(t.maxSAD)
	earlyExit := int64(maxSAD * (1.0 - options.Threshold + 0.05))
	workers := imageMatchResolveWorkers(options.Workers)

	totalRows := searchH/imageMatchCoarseStep + 1
	rowsPerWorker := (totalRows + workers - 1) / workers

	var hits []imageMatchRawHit
	var hitsMu sync.Mutex
	var wg sync.WaitGroup

	for worker := 0; worker < workers; worker++ {
		startRow := worker * rowsPerWorker
		if startRow >= totalRows {
			break
		}
		endRow := startRow + rowsPerWorker
		if endRow > totalRows {
			endRow = totalRows
		}

		wg.Add(1)
		go func(startRow, endRow int) {
			defer wg.Done()
			local := make([]imageMatchRawHit, 0)
			for row := startRow; row < endRow; row++ {
				y := row * imageMatchCoarseStep
				for x := 0; x <= searchW; x += imageMatchCoarseStep {
					diff := imageMatchSADRGB(f.rgb, f.width, x, y, t, imageMatchSampleStep, earlyExit)
					score := 1.0 - float64(diff)/maxSAD
					if score >= options.Threshold-0.1 {
						local = append(local, imageMatchRawHit{x: x, y: y, score: score})
					}
				}
			}
			if len(local) > 0 {
				hitsMu.Lock()
				hits = append(hits, local...)
				hitsMu.Unlock()
			}
		}(startRow, endRow)
	}
	wg.Wait()

	if len(hits) == 0 {
		return []ImageMatchResult{}, nil
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].y != hits[j].y {
			return hits[i].y < hits[j].y
		}
		return hits[i].x < hits[j].x
	})
	clusterRadius := imageMatchMaxInt(t.width/2, t.height/2)
	if clusterRadius < imageMatchCoarseStep {
		clusterRadius = imageMatchCoarseStep
	}
	clusterReps := imageMatchSuppressRawHits(hits, clusterRadius)

	type refinedHit struct {
		x, y  int
		score float64
	}
	refinedHits := make([]refinedHit, 0, len(clusterReps))
	for _, hit := range clusterReps {
		y0 := imageMatchClamp(hit.y-imageMatchFineRadius, 0, searchH)
		y1 := imageMatchClamp(hit.y+imageMatchFineRadius, 0, searchH)
		x0 := imageMatchClamp(hit.x-imageMatchFineRadius, 0, searchW)
		x1 := imageMatchClamp(hit.x+imageMatchFineRadius, 0, searchW)

		bestX, bestY, bestScore := hit.x, hit.y, hit.score
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				diff := imageMatchSADRGB(f.rgb, f.width, x, y, t, imageMatchSampleStep, earlyExit)
				score := 1.0 - float64(diff)/maxSAD
				if score > bestScore {
					bestScore = score
					bestX, bestY = x, y
				}
			}
		}

		bestScore = imageMatchPenalizeFlatArea(f.rgb, f.width, bestX, bestY, t, bestScore)
		if bestScore >= options.Threshold {
			refinedHits = append(refinedHits, refinedHit{x: bestX, y: bestY, score: bestScore})
		}
	}

	if len(refinedHits) == 0 {
		return []ImageMatchResult{}, nil
	}

	sort.Slice(refinedHits, func(i, j int) bool {
		if refinedHits[i].score != refinedHits[j].score {
			return refinedHits[i].score > refinedHits[j].score
		}
		if refinedHits[i].y != refinedHits[j].y {
			return refinedHits[i].y < refinedHits[j].y
		}
		return refinedHits[i].x < refinedHits[j].x
	})
	results := make([]ImageMatchResult, 0, len(refinedHits))
	for _, hit := range refinedHits {
		centerX := hit.x + t.width/2
		centerY := hit.y + t.height/2
		duplicate := false
		for _, kept := range results {
			dx := imageMatchAbsInt(centerX - kept.X)
			dy := imageMatchAbsInt(centerY - kept.Y)
			if dx < clusterRadius && dy < clusterRadius {
				duplicate = true
				break
			}
		}
		if !duplicate {
			results = append(results, ImageMatchResult{X: centerX, Y: centerY, Score: hit.score})
		}
	}

	return results, nil
}

func imageMatchValidateOptions(options ImageMatchOptions) error {
	if math.IsNaN(options.Threshold) || math.IsInf(options.Threshold, 0) || options.Threshold < 0 || options.Threshold > 1 {
		return ErrInvalidMatchThreshold
	}
	return nil
}

func imageMatchResolveWorkers(requested int) int {
	cpus := runtime.NumCPU()
	if cpus < 1 {
		cpus = 1
	}
	if requested <= 0 {
		requested = cpus / 4
		if requested < 1 {
			requested = 1
		}
		if requested > 4 {
			requested = 4
		}
	} else if requested > cpus {
		requested = cpus
	}
	return requested
}

func imageMatchSADRGB(src []uint8, srcW, sx, sy int, tpl *ImageTemplate, step int, earlyExit int64) int64 {
	var diff int64
	step3 := step * 3
	step4 := step * 4

	for ty := 0; ty < tpl.height; ty += step {
		sourceIndex := ((sy+ty)*srcW + sx) * 3
		templateIndex := (ty * tpl.width) * 4

		for tx := 0; tx < tpl.width; tx += step {
			if tpl.hasAlpha && tpl.rgbaData[templateIndex+3] < 128 {
				sourceIndex += step3
				templateIndex += step4
				continue
			}

			dr := int64(src[sourceIndex]) - int64(tpl.rgbaData[templateIndex])
			dg := int64(src[sourceIndex+1]) - int64(tpl.rgbaData[templateIndex+1])
			db := int64(src[sourceIndex+2]) - int64(tpl.rgbaData[templateIndex+2])
			diff += imageMatchAbsInt64(dr) + imageMatchAbsInt64(dg) + imageMatchAbsInt64(db)

			sourceIndex += step3
			templateIndex += step4
		}
		if earlyExit > 0 && diff > earlyExit {
			return earlyExit + 1
		}
	}
	return diff
}

// penalizeFlatArea 对匹配区域的有效像素计算方差，降低纯色区域误匹配的分数。
// 对透明模板，只统计模板中 alpha >= 128 的位置，避免透明边缘背景影响方差。
func imageMatchPenalizeFlatArea(src []uint8, srcW, sx, sy int, tpl *ImageTemplate, originalScore float64) float64 {
	if originalScore < 0.2 {
		return originalScore
	}

	step := 8
	if tpl.width > 100 || tpl.height > 100 {
		step = 16
	}
	if tpl.width > 300 || tpl.height > 300 {
		step = 24
	}

	var count float64
	var meanR, meanG, meanB float64
	var m2R, m2G, m2B float64

	for ty := 0; ty < tpl.height; ty += step {
		sourceRow := ((sy+ty)*srcW + sx) * 3
		templateRow := ty * tpl.width * 4
		for tx := 0; tx < tpl.width; tx += step {
			templateIndex := templateRow + tx*4
			// 与 SAD 匹配使用同一透明掩码，避免透明背景参与平坦区域惩罚。
			if tpl.hasAlpha && tpl.rgbaData[templateIndex+3] < 128 {
				continue
			}

			sourceIndex := sourceRow + tx*3
			r := float64(src[sourceIndex])
			g := float64(src[sourceIndex+1])
			b := float64(src[sourceIndex+2])
			count++

			deltaR := r - meanR
			meanR += deltaR / count
			m2R += deltaR * (r - meanR)

			deltaG := g - meanG
			meanG += deltaG / count
			m2G += deltaG * (g - meanG)

			deltaB := b - meanB
			meanB += deltaB / count
			m2B += deltaB * (b - meanB)
		}
	}

	if count < 4 {
		return originalScore
	}
	variance := (m2R + m2G + m2B) / count
	if variance >= imageMatchFlatVarianceThreshold {
		return originalScore
	}

	penalty := (imageMatchFlatVarianceThreshold - variance) / imageMatchFlatVarianceThreshold * imageMatchMaxFlatPenalty
	score := originalScore - penalty
	if score < 0 {
		return 0
	}
	return score
}

func imageMatchFillRGBA(img image.Image, rgba []uint8) bool {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	hasAlpha := false

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, a := img.At(x+bounds.Min.X, y+bounds.Min.Y).RGBA()
			index := (y*width + x) * 4
			rgba[index] = uint8(r >> 8)
			rgba[index+1] = uint8(g >> 8)
			rgba[index+2] = uint8(b >> 8)
			rgba[index+3] = uint8(a >> 8)
			if rgba[index+3] < 255 {
				hasAlpha = true
			}
		}
	}
	return hasAlpha
}

func imageMatchFillRGB(img image.Image, rgb []uint8) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	switch m := img.(type) {
	case *image.NRGBA:
		if bounds.Min.X == 0 && bounds.Min.Y == 0 && m.Stride == width*4 {
			for sourceIndex, targetIndex := 0, 0; sourceIndex < len(m.Pix); sourceIndex, targetIndex = sourceIndex+4, targetIndex+3 {
				rgb[targetIndex] = m.Pix[sourceIndex]
				rgb[targetIndex+1] = m.Pix[sourceIndex+1]
				rgb[targetIndex+2] = m.Pix[sourceIndex+2]
			}
			return
		}
		for y := 0; y < height; y++ {
			sourceRow := m.PixOffset(bounds.Min.X, y+bounds.Min.Y)
			targetRow := y * width * 3
			for x := 0; x < width; x++ {
				si := sourceRow + x*4
				di := targetRow + x*3
				rgb[di], rgb[di+1], rgb[di+2] = m.Pix[si], m.Pix[si+1], m.Pix[si+2]
			}
		}
	case *image.RGBA:
		if bounds.Min.X == 0 && bounds.Min.Y == 0 && m.Stride == width*4 {
			for sourceIndex, targetIndex := 0, 0; sourceIndex < len(m.Pix); sourceIndex, targetIndex = sourceIndex+4, targetIndex+3 {
				rgb[targetIndex] = m.Pix[sourceIndex]
				rgb[targetIndex+1] = m.Pix[sourceIndex+1]
				rgb[targetIndex+2] = m.Pix[sourceIndex+2]
			}
			return
		}
		for y := 0; y < height; y++ {
			sourceRow := m.PixOffset(bounds.Min.X, y+bounds.Min.Y)
			targetRow := y * width * 3
			for x := 0; x < width; x++ {
				si := sourceRow + x*4
				di := targetRow + x*3
				rgb[di], rgb[di+1], rgb[di+2] = m.Pix[si], m.Pix[si+1], m.Pix[si+2]
			}
		}
	case *image.YCbCr:
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				yy := m.Y[m.YOffset(x+bounds.Min.X, y+bounds.Min.Y)]
				cb := m.Cb[m.COffset(x+bounds.Min.X, y+bounds.Min.Y)]
				cr := m.Cr[m.COffset(x+bounds.Min.X, y+bounds.Min.Y)]
				r, g, b := color.YCbCrToRGB(yy, cb, cr)
				index := (y*width + x) * 3
				rgb[index], rgb[index+1], rgb[index+2] = r, g, b
			}
		}
	default:
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				r, g, b, _ := img.At(x+bounds.Min.X, y+bounds.Min.Y).RGBA()
				index := (y*width + x) * 3
				rgb[index], rgb[index+1], rgb[index+2] = uint8(r>>8), uint8(g>>8), uint8(b>>8)
			}
		}
	}
}

func imageMatchSuppressRawHits(hits []imageMatchRawHit, radius int) []imageMatchRawHit {
	if len(hits) == 0 {
		return nil
	}
	if radius < 1 {
		radius = 1
	}

	type cell struct{ x, y int }
	grid := make(map[cell][]imageMatchRawHit)
	kept := make([]imageMatchRawHit, 0, len(hits))
	for _, hit := range hits {
		key := cell{x: hit.x / radius, y: hit.y / radius}
		duplicate := false
		for cy := key.y - 1; cy <= key.y+1 && !duplicate; cy++ {
			for cx := key.x - 1; cx <= key.x+1 && !duplicate; cx++ {
				for _, candidate := range grid[cell{x: cx, y: cy}] {
					if imageMatchAbsInt(hit.x-candidate.x) < radius && imageMatchAbsInt(hit.y-candidate.y) < radius {
						duplicate = true
						break
					}
				}
			}
		}
		if !duplicate {
			kept = append(kept, hit)
			grid[key] = append(grid[key], hit)
		}
	}
	return kept
}

func imageMatchClamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func imageMatchAbsInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func imageMatchAbsInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func imageMatchMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func imageMatchMaxIntValue() int {
	return int(^uint(0) >> 1)
}

func imageMatchMaxIntOf() int {
	return imageMatchMaxIntValue()
}

// -----------------------------------------------------------------------------
// Device 便捷找图 API
//
// Gouia 的 automator.Device 匿名嵌入 *libs.Device，因此这里定义的方法会自动提升到
// automator.Device。业务代码可以直接调用 d.FindImage / d.FindImageMulti，Gouia
// 不需要再维护一份图像匹配适配代码。
// -----------------------------------------------------------------------------

// DefaultImageTemplateDir 是设备找图 API 默认使用的模板目录。
// 路径相对于进程当前工作目录，例如 Gouia 项目根目录下的 find_pic。
const DefaultImageTemplateDir = "find_pic"

const (
	imageMatchDirectoryCacheTTL = 30 * time.Second
	imageMatchRetryInterval     = 300 * time.Millisecond
	imageMatchMultiRetryDelay   = 500 * time.Millisecond
	imageMatchVariantNMSRadius  = 20
)

type imageMatchTemplateFile struct {
	path     string
	nameIdx  int
	template *ImageTemplate
}

type imageMatchTemplateCacheEntry struct {
	size    int64
	modTime int64
	tpl     *ImageTemplate
}

type imageMatchDirectoryCacheEntry struct {
	files  []string
	expiry time.Time
}

var (
	imageMatchTemplateCacheMu sync.RWMutex
	imageMatchTemplateCache   = make(map[string]imageMatchTemplateCacheEntry)
	imageMatchDirectoryMu     sync.Mutex
	imageMatchDirectoryCache  = make(map[string]imageMatchDirectoryCacheEntry)

	imageMatchTemplateDirMu sync.RWMutex
	imageMatchTemplateDir   = DefaultImageTemplateDir
)

// MatchPoint 表示多目标找图返回的一个坐标和相似度。
// X、Y 是像素坐标；FindImageMulti 的 usePerc=true 时会转换为 0~1 的比例坐标。
type MatchPoint struct {
	X     float64
	Y     float64
	Score float64
}

// SetImageTemplateDir 修改 FindImage 和 FindImageMulti 使用的默认模板目录。
// 不调用时默认使用当前工作目录下的 find_pic。
// 传入相对路径时，相对于进程当前工作目录解析。
func SetImageTemplateDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("图片模板目录不能为空")
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("解析图片模板目录失败: %w", err)
	}

	imageMatchTemplateDirMu.Lock()
	imageMatchTemplateDir = absDir
	imageMatchTemplateDirMu.Unlock()

	imageMatchDirectoryMu.Lock()
	imageMatchDirectoryCache = make(map[string]imageMatchDirectoryCacheEntry)
	imageMatchDirectoryMu.Unlock()
	return nil
}

// GetImageTemplateDir 返回当前找图模板目录。
func GetImageTemplateDir() string {
	imageMatchTemplateDirMu.RLock()
	dir := imageMatchTemplateDir
	imageMatchTemplateDirMu.RUnlock()

	absDir, err := filepath.Abs(dir)
	if err == nil {
		return absDir
	}
	return dir
}

// ClearImageTemplateCache 清除模板图片和目录列表缓存。
// 更换模板文件后通常会自动按文件大小和修改时间刷新；新增或删除模板文件后，
// 如需立即生效，可以主动调用此方法，不必等待目录缓存过期。
func ClearImageTemplateCache() {
	imageMatchTemplateCacheMu.Lock()
	imageMatchTemplateCache = make(map[string]imageMatchTemplateCacheEntry)
	imageMatchTemplateCacheMu.Unlock()
	imageMatchDirectoryMu.Lock()
	imageMatchDirectoryCache = make(map[string]imageMatchDirectoryCacheEntry)
	imageMatchDirectoryMu.Unlock()
}

// FindImage 在设备当前屏幕中查找一个或多个模板名称。
//
// targetNames 支持 string 或 []string；当传入 []string 时，按数组顺序确定返回索引，
// 即优先返回最靠前名称的命中结果。同一名称可对应多张 PNG/JPG/JPEG/GIF 模板变体。
// threshold 为 0~1 的匹配阈值；usePerc=true 时返回屏幕宽高比例坐标。
// maxRetry 不传或传入非正数时默认重试 30 次。
// 未找到或截图/模板错误时返回 (-1, -1, -1)，错误会写入标准日志。
//
// 该方法直接使用 libs.Device.Screenshot，无需 Gouia 再包装截图接口。
func (d *Device) FindImage(targetNames interface{}, threshold float64, usePerc bool, maxRetry ...int) (float64, float64, int) {
	if d == nil {
		log.Printf("[Gua FindImage] device is nil")
		return -1, -1, -1
	}
	if err := imageMatchValidateOptions(ImageMatchOptions{Threshold: threshold}); err != nil {
		log.Printf("[Gua FindImage] invalid threshold %.4f: %v", threshold, err)
		return -1, -1, -1
	}

	names, err := imageMatchNormalizeNames(targetNames)
	if err != nil {
		log.Printf("[Gua FindImage] invalid target names: %v", err)
		return -1, -1, -1
	}

	retries := 30
	if len(maxRetry) > 0 && maxRetry[0] > 0 {
		retries = maxRetry[0]
	}

	candidates, err := imageMatchLoadCandidates(names)
	if err != nil {
		log.Printf("[Gua FindImage] load templates failed: %v", err)
		return -1, -1, -1
	}
	if len(candidates) == 0 {
		log.Printf("[Gua FindImage] no template files found: %v (dir=%s)", names, GetImageTemplateDir())
		return -1, -1, -1
	}

	workers := imageMatchResolveWorkers(0)
	semaphore := make(chan struct{}, workers)
	lastScreen := make([]byte, 0)

	for attempt := 1; attempt <= retries; attempt++ {
		screenData, screenshotErr := d.Screenshot()
		if screenshotErr != nil {
			log.Printf("[Gua FindImage] screenshot failed (%d/%d): %v", attempt, retries, screenshotErr)
			if attempt < retries {
				time.Sleep(imageMatchRetryInterval)
			}
			continue
		}

		// 只有截图字节完全相同时才跳过重复扫描，不使用稀疏指纹，避免漏掉局部变化。
		if len(lastScreen) > 0 && bytes.Equal(screenData, lastScreen) {
			if attempt < retries {
				time.Sleep(imageMatchRetryInterval)
			}
			continue
		}

		frame, frameErr := NewImageFrame(screenData)
		if frameErr != nil {
			log.Printf("[Gua FindImage] decode screenshot failed (%d/%d): %v", attempt, retries, frameErr)
			if attempt < retries {
				time.Sleep(imageMatchRetryInterval)
			}
			continue
		}

		type matchOutcome struct {
			result  ImageMatchResult
			nameIdx int
			err     error
		}
		outcomes := make([]matchOutcome, len(candidates))
		var wg sync.WaitGroup

		for index, candidate := range candidates {
			wg.Add(1)
			go func(index int, candidate imageMatchTemplateFile) {
				defer wg.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				result, matchErr := frame.Match(candidate.template, ImageMatchOptions{Threshold: threshold})
				outcomes[index] = matchOutcome{
					result:  result,
					nameIdx: candidate.nameIdx,
					err:     matchErr,
				}
			}(index, candidate)
		}
		wg.Wait()

		found := false
		best := matchOutcome{}
		for _, outcome := range outcomes {
			if outcome.err != nil {
				continue
			}
			if !found || outcome.nameIdx < best.nameIdx ||
				(outcome.nameIdx == best.nameIdx && outcome.result.Score > best.result.Score) {
				best = outcome
				found = true
			}
		}

		if found {
			width, height := frame.Size()
			x, y := float64(best.result.X), float64(best.result.Y)
			if usePerc {
				x /= float64(width)
				y /= float64(height)
			}
			return x, y, best.nameIdx
		}

		lastScreen = append(lastScreen[:0], screenData...)
		if attempt < retries {
			time.Sleep(imageMatchRetryInterval)
		}
	}

	return -1, -1, -1
}

// FindImageMulti 在设备当前屏幕中查找同一模板的所有匹配位置。
//
// targetName 是模板文件名前缀，同名前缀的 PNG/JPG/JPEG/GIF 变体都会参与匹配。
// maxResults <= 0 表示不限制返回数量；maxRetry 不传或传入非正数时默认重试 1 次。
// usePerc=true 时 X、Y 返回屏幕比例坐标（0~1）。
// 没有找到时返回空结果和 nil；截图失败等错误会返回 error。
func (d *Device) FindImageMulti(targetName string, threshold float64, usePerc bool, maxResults int, maxRetry ...int) ([]MatchPoint, error) {
	if d == nil {
		err := errors.New("device is nil")
		log.Printf("[Gua FindImageMulti] %v", err)
		return nil, err
	}
	targetName = strings.TrimSpace(targetName)
	if targetName == "" {
		err := errors.New("targetName 不能为空")
		log.Printf("[Gua FindImageMulti] %v", err)
		return nil, err
	}
	if err := imageMatchValidateOptions(ImageMatchOptions{Threshold: threshold}); err != nil {
		log.Printf("[Gua FindImageMulti] invalid threshold %.4f: %v", threshold, err)
		return nil, err
	}

	retries := 1
	if len(maxRetry) > 0 && maxRetry[0] > 0 {
		retries = maxRetry[0]
	}

	root := GetImageTemplateDir()
	dir := imageMatchResolveSearchDir(root, targetName)
	files, err := imageMatchFindTemplateFiles(dir, targetName)
	if err != nil {
		log.Printf("[Gua FindImageMulti] read template directory failed %s: %v", dir, err)
		return nil, err
	}
	if len(files) == 0 {
		err := fmt.Errorf("找不到模板文件: %s", targetName)
		log.Printf("[Gua FindImageMulti] %v (dir=%s)", err, dir)
		return nil, err
	}

	templates := make([]imageMatchTemplateFile, 0, len(files))
	for _, path := range files {
		tpl, loadErr := imageMatchLoadTemplate(path)
		if loadErr != nil {
			log.Printf("[Gua FindImageMulti] load template failed %s: %v", path, loadErr)
			continue
		}
		templates = append(templates, imageMatchTemplateFile{path: path, template: tpl})
	}
	if len(templates) == 0 {
		err := fmt.Errorf("没有可用的模板图片: %s", targetName)
		log.Printf("[Gua FindImageMulti] %v", err)
		return nil, err
	}

	var lastErr error
	lastScreen := make([]byte, 0)
	for attempt := 1; attempt <= retries; attempt++ {
		screenData, screenshotErr := d.Screenshot()
		if screenshotErr != nil {
			lastErr = screenshotErr
			log.Printf("[Gua FindImageMulti] screenshot failed (%d/%d): %v", attempt, retries, screenshotErr)
			if attempt < retries {
				time.Sleep(imageMatchRetryInterval)
			}
			continue
		}

		if len(lastScreen) > 0 && bytes.Equal(screenData, lastScreen) {
			if attempt < retries {
				time.Sleep(imageMatchMultiRetryDelay)
			}
			continue
		}

		frame, frameErr := NewImageFrame(screenData)
		if frameErr != nil {
			lastErr = frameErr
			if attempt < retries {
				time.Sleep(imageMatchRetryInterval)
			}
			continue
		}

		all := make([]MatchPoint, 0)
		for _, item := range templates {
			points, matchErr := frame.MatchAll(item.template, ImageMatchOptions{
				Threshold: threshold,
				Workers:   imageMatchResolveWorkers(0),
			})
			if matchErr != nil {
				lastErr = matchErr
				log.Printf("[Gua FindImageMulti] match template failed %s: %v", item.path, matchErr)
				continue
			}
			for _, point := range points {
				all = append(all, MatchPoint{
					X:     float64(point.X),
					Y:     float64(point.Y),
					Score: point.Score,
				})
			}
		}

		if len(all) == 0 {
			lastScreen = append(lastScreen[:0], screenData...)
			if attempt < retries {
				time.Sleep(imageMatchMultiRetryDelay)
				continue
			}
			return []MatchPoint{}, nil
		}

		// 模板变体之间再次去重，保留相似度最高的坐标。
		sort.Slice(all, func(i, j int) bool {
			if all[i].Score != all[j].Score {
				return all[i].Score > all[j].Score
			}
			if all[i].Y != all[j].Y {
				return all[i].Y < all[j].Y
			}
			return all[i].X < all[j].X
		})

		final := make([]MatchPoint, 0, len(all))
		for _, point := range all {
			duplicate := false
			for _, kept := range final {
				dx := imageMatchAbsInt(int(kept.X - point.X))
				dy := imageMatchAbsInt(int(kept.Y - point.Y))
				if dx < imageMatchVariantNMSRadius && dy < imageMatchVariantNMSRadius {
					duplicate = true
					break
				}
			}
			if !duplicate {
				final = append(final, point)
			}
		}

		// 按屏幕从上到下、从左到右排序，适合逐项点击列表。
		sort.Slice(final, func(i, j int) bool {
			if final[i].Y != final[j].Y {
				return final[i].Y < final[j].Y
			}
			return final[i].X < final[j].X
		})

		if maxResults > 0 && len(final) > maxResults {
			final = final[:maxResults]
		}

		if usePerc {
			width, height := frame.Size()
			for index := range final {
				final[index].X /= float64(width)
				final[index].Y /= float64(height)
			}
		}
		return final, nil
	}

	if lastErr != nil {
		log.Printf("[Gua FindImageMulti] search failed: %v", lastErr)
		return nil, lastErr
	}
	return []MatchPoint{}, nil
}

func imageMatchNormalizeNames(value interface{}) ([]string, error) {
	var names []string
	switch v := value.(type) {
	case string:
		names = []string{v}
	case []string:
		names = append([]string(nil), v...)
	default:
		return nil, errors.New("targetNames 必须是 string 或 []string")
	}

	if len(names) == 0 {
		return nil, errors.New("targetNames 不能为空")
	}
	for index, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("targetNames[%d] 不能为空", index)
		}
	}
	return names, nil
}

func imageMatchLoadCandidates(names []string) ([]imageMatchTemplateFile, error) {
	root := GetImageTemplateDir()
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("找图目录不可用 %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("找图路径不是目录: %s", root)
	}

	candidates := make([]imageMatchTemplateFile, 0)
	for nameIndex, name := range names {
		dir := imageMatchResolveSearchDir(root, name)
		files, readErr := imageMatchFindTemplateFiles(dir, name)
		if readErr != nil {
			log.Printf("[Gua FindImage] read template directory failed %s: %v", dir, readErr)
			continue
		}
		for _, path := range files {
			tpl, loadErr := imageMatchLoadTemplate(path)
			if loadErr != nil {
				log.Printf("[Gua FindImage] load template failed %s: %v", path, loadErr)
				continue
			}
			candidates = append(candidates, imageMatchTemplateFile{
				path:     path,
				nameIdx:  nameIndex,
				template: tpl,
			})
		}
	}
	return candidates, nil
}

func imageMatchResolveSearchDir(root, name string) string {
	if strings.Contains(name, "_") {
		subdir := strings.SplitN(name, "_", 2)[0]
		candidate := filepath.Join(root, subdir)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return root
}

func imageMatchFindTemplateFiles(dir, prefix string) ([]string, error) {
	entries := imageMatchReadDirectory(dir)
	if entries == nil {
		if _, err := os.Stat(dir); err != nil {
			return nil, err
		}
	}

	matched := make([]string, 0)
	for _, filename := range entries {
		lower := strings.ToLower(filename)
		if strings.HasPrefix(filename, prefix) &&
			(strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".jpg") ||
				strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".gif")) {
			matched = append(matched, filepath.Join(dir, filename))
		}
	}
	sort.Strings(matched)
	return matched, nil
}

func imageMatchReadDirectory(dir string) []string {
	now := time.Now()
	imageMatchDirectoryMu.Lock()
	entry, ok := imageMatchDirectoryCache[dir]
	if ok && now.Before(entry.expiry) {
		files := append([]string(nil), entry.files...)
		imageMatchDirectoryMu.Unlock()
		return files
	}

	des, err := os.ReadDir(dir)
	if err != nil {
		delete(imageMatchDirectoryCache, dir)
		imageMatchDirectoryMu.Unlock()
		return nil
	}

	files := make([]string, 0, len(des))
	for _, item := range des {
		if !item.IsDir() {
			files = append(files, item.Name())
		}
	}
	imageMatchDirectoryCache[dir] = imageMatchDirectoryCacheEntry{
		files:  files,
		expiry: now.Add(imageMatchDirectoryCacheTTL),
	}
	result := append([]string(nil), files...)
	imageMatchDirectoryMu.Unlock()
	return result
}

func imageMatchLoadTemplate(path string) (*ImageTemplate, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("模板不是普通文件: %s", path)
	}

	modTime := info.ModTime().UnixNano()
	imageMatchTemplateCacheMu.RLock()
	entry, cached := imageMatchTemplateCache[path]
	imageMatchTemplateCacheMu.RUnlock()
	if cached && entry.tpl != nil && entry.size == info.Size() && entry.modTime == modTime {
		return entry.tpl, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tpl, err := NewImageTemplate(data)
	if err != nil {
		return nil, fmt.Errorf("解码模板失败 %s: %w", path, err)
	}

	// 使用读取后的文件时间更新缓存，替换模板文件后会在下次调用时重新解码。
	latestInfo, statErr := os.Stat(path)
	if statErr == nil {
		info = latestInfo
	}
	imageMatchTemplateCacheMu.Lock()
	imageMatchTemplateCache[path] = imageMatchTemplateCacheEntry{
		size:    info.Size(),
		modTime: info.ModTime().UnixNano(),
		tpl:     tpl,
	}
	imageMatchTemplateCacheMu.Unlock()
	return tpl, nil
}

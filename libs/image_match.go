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
	"math"
	"runtime"
	"sort"
	"sync"
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

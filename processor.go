package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	imagedraw "image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/corona10/goimagehash"
	webp "github.com/gen2brain/webp"
	exifpkg "github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
	xdraw "golang.org/x/image/draw"
	xtiff "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const sourceGenerationMetadataKey = "sourceGeneration"

var derivedObjectPattern = regexp.MustCompile(`-w\d{2,}`)

var (
	updateImageMetadata   = UpdateImageMetadata
	updateImageVectorOnly = UpdateImageVectorOnly
	computeImageVector    = ComputeImageVector
)

type Processor struct {
	cfg       Config
	storage   *storage.Client
	watermark *image.NRGBA
}

func NewProcessor(cfg Config, storageClient *storage.Client) (*Processor, error) {
	p := &Processor{
		cfg:     cfg,
		storage: storageClient,
	}
	if !cfg.EnableWatermark {
		return p, nil
	}

	wmBytes, err := os.ReadFile(cfg.WatermarkPath)
	if err != nil {
		return nil, fmt.Errorf("read watermark: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(wmBytes))
	if err != nil {
		return nil, fmt.Errorf("decode watermark: %w", err)
	}
	p.watermark = toNRGBA(img)
	return p, nil
}

func (p *Processor) Process(ctx context.Context, event storageEvent) error {
	if !isSupportedImage(event.Name) {
		log.Printf("skip unsupported object: %s", event.Name)
		return nil
	}
	if derivedObjectPattern.MatchString(event.Name) {
		log.Printf("skip derived object: %s", event.Name)
		return nil
	}

	base := filepath.Base(event.Name)
	ext := filepath.Ext(base)
	nameWithoutExt := strings.TrimSuffix(base, filepath.Ext(base))
	baseDir := strings.TrimSuffix(event.Name, base)

	originalWebPName := baseDir + nameWithoutExt + ".webP"
	completionSentinelName := completionSentinelObjectName(baseDir, nameWithoutExt, originalWebPName, p.cfg.ResizeTargets)
	alreadyProcessed, err := p.alreadyProcessedSourceGeneration(ctx, event.Bucket, completionSentinelName, event.Generation)
	if err != nil {
		return err
	}
	if alreadyProcessed {
		log.Printf("skip already processed source generation: object=%s generation=%s", event.Name, event.Generation)
		return nil
	}

	reader, err := p.storage.Bucket(event.Bucket).Object(event.Name).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("open object: %w", err)
	}
	defer reader.Close()

	originalBytes, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}
	if p.cfg.MaxSourcePixels > 0 {
		if imgCfg, _, err := image.DecodeConfig(bytes.NewReader(originalBytes)); err == nil {
			if err := validateSourceImageSize(imgCfg.Width, imgCfg.Height, p.cfg.MaxSourcePixels); err != nil {
				return err
			}
		}
	}
	sourceImg, _, err := image.Decode(bytes.NewReader(originalBytes))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	sourceImg = applyEXIFOrientation(sourceImg, originalBytes)
	exifMap := extractAllEXIF(originalBytes)

	originalWebPBytes, err := encodeWebP(sourceImg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", originalWebPName, err)
	}
	if err := p.uploadObject(ctx, event.Bucket, originalWebPName, "image/webp", originalWebPBytes, event.Generation); err != nil {
		return err
	}

	for _, target := range p.cfg.ResizeTargets {
		resized := resizeImage(sourceImg, target.Width)
		if p.cfg.EnableWatermark {
			resized = applyWatermark(resized, p.watermark, p.cfg.WatermarkScale, p.cfg.WatermarkMarginRatio, p.cfg.WatermarkOpacity)
		}

		mainObjectName := baseDir + nameWithoutExt + "-" + target.Label + ext
		mainBytes, err := encodeByExt(resized, ext)
		if err != nil {
			return fmt.Errorf("encode %s: %w", mainObjectName, err)
		}
		if err := p.uploadObject(ctx, event.Bucket, mainObjectName, contentTypeFromExt(ext), mainBytes, event.Generation); err != nil {
			return err
		}

		webpObjectName := baseDir + nameWithoutExt + "-" + target.Label + ".webP"
		webpBytes, err := encodeWebP(resized)
		if err != nil {
			return fmt.Errorf("encode %s: %w", webpObjectName, err)
		}
		if err := p.uploadObject(ctx, event.Bucket, webpObjectName, "image/webp", webpBytes, event.Generation); err != nil {
			return err
		}

		if target.Label == "w480" {
			p.handleW480Metadata(event.Name, event.Bucket, nameWithoutExt, resized, exifMap, mainBytes)
		}

		mainBytes = nil
		webpBytes = nil
	}

	return nil
}

func (p *Processor) alreadyProcessedSourceGeneration(ctx context.Context, bucketName, sentinelObjectName, sourceGeneration string) (bool, error) {
	if sourceGeneration == "" {
		return false, nil
	}

	attrs, err := p.storage.Bucket(bucketName).Object(sentinelObjectName).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check sentinel object %s: %w", sentinelObjectName, err)
	}
	return attrs.Metadata[sourceGenerationMetadataKey] == sourceGeneration, nil
}

func completionSentinelObjectName(baseDir, imageFileID, fallback string, targets []ResizeTarget) string {
	if len(targets) == 0 {
		return fallback
	}
	return baseDir + imageFileID + "-" + targets[len(targets)-1].Label + ".webP"
}

func (p *Processor) handleW480Metadata(eventName, bucketName, imageFileID string, resized image.Image, exifMap map[string]interface{}, encodedW480Bytes []byte) {
	hash, err := goimagehash.PerceptionHash(resized)
	if err != nil {
		log.Printf("failed to compute phash for %s: %v", eventName, err)
		return
	}
	phashStr := fmt.Sprintf("%016x", hash.GetHash())

	if err := updateImageMetadata(p.cfg, imageFileID, phashStr, bucketName, exifMap, nil); err != nil {
		log.Printf("failed to update image metadata for %s: %v", eventName, err)
	}

	if !p.cfg.EnableImageVector {
		return
	}

	vectorPayload := append([]byte(nil), encodedW480Bytes...)
	go p.computeAndUpdateImageVector(eventName, bucketName, imageFileID, phashStr, exifMap, vectorPayload)
}

func (p *Processor) computeAndUpdateImageVector(eventName, bucketName, imageFileID, phashStr string, exifMap map[string]interface{}, encodedW480Bytes []byte) {
	vec, err := computeImageVector(encodedW480Bytes)
	if err != nil {
		log.Printf("failed to compute image vector for %s: %v", eventName, err)
		return
	}
	if len(vec) == 0 {
		log.Printf("computed empty image vector for %s", eventName)
		return
	}
	if err := updateImageMetadata(p.cfg, imageFileID, phashStr, bucketName, exifMap, vec); err != nil {
		log.Printf("failed to update image vector for %s: %v", eventName, err)
	}
}

func (p *Processor) BackfillImageVectorFromObject(ctx context.Context, bucketName, objectName, imageFileID string) error {
	reader, err := p.storage.Bucket(bucketName).Object(objectName).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("open object: %w", err)
	}
	defer reader.Close()

	imageBytes, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read object: %w", err)
	}

	vector, err := computeImageVector(imageBytes)
	if err != nil {
		return fmt.Errorf("compute vector: %w", err)
	}
	if len(vector) == 0 {
		return fmt.Errorf("computed empty image vector")
	}

	if err := updateImageVectorOnly(p.cfg, imageFileID, vector); err != nil {
		return fmt.Errorf("update image vector: %w", err)
	}
	return nil
}

func (p *Processor) uploadObject(ctx context.Context, bucketName, objectName, contentType string, payload []byte, sourceGeneration string) error {
	writer := p.storage.Bucket(bucketName).Object(objectName).NewWriter(ctx)
	writer.ContentType = contentType
	writer.CacheControl = p.cfg.CacheControl
	if sourceGeneration != "" {
		writer.Metadata = map[string]string{
			sourceGenerationMetadataKey: sourceGeneration,
		}
	}

	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write object %s: %w", objectName, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close object %s: %w", objectName, err)
	}

	log.Printf("uploaded gs://%s/%s", bucketName, objectName)
	return nil
}

func isSupportedImage(name string) bool {
	ext := filepath.Ext(name)
	if ext == ".webP" {
		return false
	}

	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".tif", ".tiff", ".webp":
		return true
	default:
		return false
	}
}

func resizeImage(src image.Image, targetWidth int) *image.NRGBA {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width == 0 || height == 0 || targetWidth <= 0 {
		return toNRGBA(src)
	}

	targetHeight := height * targetWidth / width
	if targetHeight <= 0 {
		targetHeight = 1
	}

	dst := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
	return dst
}

func validateSourceImageSize(width, height, maxPixels int) error {
	if maxPixels <= 0 || width <= 0 || height <= 0 {
		return nil
	}
	pixels := int64(width) * int64(height)
	if pixels <= int64(maxPixels) {
		return nil
	}
	return fmt.Errorf("source image is too large: %dx%d=%d pixels exceeds max %d", width, height, pixels, maxPixels)
}

func applyWatermark(base *image.NRGBA, watermark *image.NRGBA, scale, marginRatio, opacity float64) *image.NRGBA {
	if base == nil || watermark == nil {
		return base
	}
	if scale <= 0 {
		scale = 0.15
	}
	if marginRatio < 0 {
		marginRatio = 0
	}
	if opacity <= 0 {
		opacity = 1
	}
	if opacity > 1 {
		opacity = 1
	}

	targetWidth := int(float64(base.Bounds().Dx()) * scale)
	if targetWidth < 1 {
		targetWidth = 1
	}

	scaled := resizeImage(watermark, targetWidth)
	if opacity < 1 {
		scaled = adjustOpacity(scaled, opacity)
	}

	margin := int(float64(base.Bounds().Dx()) * marginRatio)
	x := base.Bounds().Dx() - scaled.Bounds().Dx() - margin
	y := base.Bounds().Dy() - scaled.Bounds().Dy() - margin
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	result := cloneNRGBA(base)
	rect := image.Rect(x, y, x+scaled.Bounds().Dx(), y+scaled.Bounds().Dy())
	imagedraw.Draw(result, rect, scaled, image.Point{}, imagedraw.Over)
	return result
}

func adjustOpacity(img *image.NRGBA, opacity float64) *image.NRGBA {
	out := cloneNRGBA(img)
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = uint8(float64(out.Pix[i]) * opacity)
	}
	return out
}

func encodeByExt(img image.Image, ext string) ([]byte, error) {
	var buf bytes.Buffer
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		if err := jpeg.Encode(&buf, flattenIfNeeded(img), &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
	case ".png":
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
	case ".gif":
		if err := gif.Encode(&buf, flattenIfNeeded(img), nil); err != nil {
			return nil, err
		}
	case ".tif", ".tiff":
		if err := xtiff.Encode(&buf, img, nil); err != nil {
			return nil, err
		}
	case ".webp":
		return encodeWebP(img)
	default:
		if err := jpeg.Encode(&buf, flattenIfNeeded(img), &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func applyEXIFOrientation(img image.Image, data []byte) image.Image {
	orientation := exifOrientation(data)
	switch orientation {
	case 3:
		return rotate180(img)
	case 6:
		return rotate90CW(img)
	case 8:
		return rotate90CCW(img)
	default:
		return img
	}
}

func exifOrientation(data []byte) int {
	exifData, err := exifpkg.Decode(bytes.NewReader(data))
	if err != nil {
		return 1
	}
	tag, err := exifData.Get(exifpkg.Orientation)
	if err != nil {
		return 1
	}
	orientation, err := tag.Int(0)
	if err != nil {
		return 1
	}
	return orientation
}

type exifWalker struct {
	Data map[string]interface{}
}

func (w *exifWalker) Walk(name exifpkg.FieldName, tag *tiff.Tag) error {
	w.Data[string(name)] = tag.String()
	return nil
}

func extractAllEXIF(data []byte) map[string]interface{} {
	exifData, err := exifpkg.Decode(bytes.NewReader(data))
	if err != nil {
		return map[string]interface{}{}
	}
	walker := &exifWalker{Data: make(map[string]interface{})}
	exifData.Walk(walker)
	return walker.Data
}

func rotate180(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(width-1-x, height-1-y, img.At(x, y))
		}
	}
	return dst
}

func rotate90CW(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(height-1-y, x, img.At(x, y))
		}
	}
	return dst
}

func rotate90CCW(src image.Image) *image.NRGBA {
	img := toNRGBA(src)
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dst.Set(y, width-1-x, img.At(x, y))
		}
	}
	return dst
}

func encodeWebP(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	err := webp.Encode(&buf, img, webp.Options{
		Quality: 85,
		Method:  4,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func flattenIfNeeded(img image.Image) image.Image {
	nrgba := toNRGBA(img)
	if !hasAlpha(nrgba) {
		return nrgba
	}
	rgba := image.NewRGBA(nrgba.Bounds())
	imagedraw.Draw(rgba, rgba.Bounds(), image.NewUniform(image.White), image.Point{}, imagedraw.Src)
	imagedraw.Draw(rgba, rgba.Bounds(), nrgba, image.Point{}, imagedraw.Over)
	return rgba
}

func hasAlpha(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return true
		}
	}
	return false
}

func toNRGBA(src image.Image) *image.NRGBA {
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	imagedraw.Draw(dst, dst.Bounds(), src, bounds.Min, imagedraw.Src)
	return dst
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

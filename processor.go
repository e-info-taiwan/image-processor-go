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
	updateImageMetadata        = UpdateImageMetadata
	updateImageVectorOnly      = UpdateImageVectorOnly
	computeImageVector         = ComputeImageVector
	detectImageLabelsWithFaces = DetectImageLabelsWithFaces
	updateImageLabels          = UpdateImageLabels
	markImageLabelsFailed      = MarkImageLabelsFailed
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
		imgCfg, _, err := image.DecodeConfig(bytes.NewReader(originalBytes))
		if err != nil {
			return fmt.Errorf("decode image config: %w", err)
		}
		if err := validateSourceImageSize(imgCfg.Width, imgCfg.Height, p.cfg.MaxSourcePixels); err != nil {
			return err
		}
	}
	sourceImg, format, err := image.Decode(bytes.NewReader(originalBytes))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	isOpaque := (strings.ToLower(format) == "jpeg")

	exifData := decodeExif(originalBytes)
	sourceImg = applyEXIFOrientation(sourceImg, exifData)
	exifMap := extractAllEXIF(exifData)

	_, err = p.encodeAndUpload(ctx, sourceImg, event.Bucket, originalWebPName, "image/webp", ".webp", event.Generation, isOpaque, false)
	if err != nil {
		return fmt.Errorf("encode and upload %s: %w", originalWebPName, err)
	}

	for _, target := range p.cfg.ResizeTargets {
		resized := resizeImage(sourceImg, target.Width)
		if p.cfg.EnableWatermark {
			resized = applyWatermark(resized, p.watermark, p.cfg.WatermarkScale, p.cfg.WatermarkMarginRatio, p.cfg.WatermarkOpacity)
		}

		mainObjectName := baseDir + nameWithoutExt + "-" + target.Label + ext
		needMainBytes := (target.Label == "w480" && (p.cfg.EnableImageVector || p.cfg.EnableImageLabel))
		mainBytes, err := p.encodeAndUpload(ctx, resized, event.Bucket, mainObjectName, contentTypeFromExt(ext), ext, event.Generation, isOpaque, needMainBytes)
		if err != nil {
			return fmt.Errorf("encode and upload %s: %w", mainObjectName, err)
		}

		webpObjectName := baseDir + nameWithoutExt + "-" + target.Label + ".webP"
		_, err = p.encodeAndUpload(ctx, resized, event.Bucket, webpObjectName, "image/webp", ".webp", event.Generation, isOpaque, false)
		if err != nil {
			return fmt.Errorf("encode and upload %s: %w", webpObjectName, err)
		}

		if target.Label == "w480" {
			p.handleW480Metadata(event.Name, event.Bucket, nameWithoutExt, resized, exifMap, mainBytes)
		}
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

	if !p.cfg.EnableImageVector && !p.cfg.EnableImageLabel {
		return
	}

	payload := append([]byte(nil), encodedW480Bytes...)
	if p.cfg.EnableImageVector {
		go p.computeAndUpdateImageVector(eventName, imageFileID, payload)
	}
	if p.cfg.EnableImageLabel {
		go p.computeAndUpdateImageLabels(eventName, imageFileID, payload)
	}
}

func (p *Processor) computeAndUpdateImageVector(eventName, imageFileID string, encodedW480Bytes []byte) {
	vec, err := computeImageVector(encodedW480Bytes)
	if err != nil {
		log.Printf("failed to compute image vector for %s: %v", eventName, err)
		return
	}
	if len(vec) == 0 {
		log.Printf("computed empty image vector for %s", eventName)
		return
	}
	if err := updateImageVectorOnly(p.cfg, imageFileID, vec); err != nil {
		log.Printf("failed to update image vector for %s: %v", eventName, err)
	}
}

func (p *Processor) computeAndUpdateImageLabels(eventName, imageFileID string, encodedW480Bytes []byte) {
	labels, hasPerson, err := detectImageLabelsWithFaces(p.cfg, encodedW480Bytes)
	if err != nil {
		log.Printf("failed to detect image labels for %s: %v", eventName, err)
		if markErr := markImageLabelsFailed(p.cfg, imageFileID, err.Error()); markErr != nil {
			log.Printf("failed to mark image labels failed for %s: %v", eventName, markErr)
		}
		return
	}

	suggestions := AddPersonSuggestion(FilterImageLabelSuggestions(labels, p.cfg.ImageLabelMinScore), hasPerson)
	if err := updateImageLabels(p.cfg, imageFileID, labels, suggestions); err != nil {
		log.Printf("failed to update image labels for %s: %v", eventName, err)
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

	rect := image.Rect(x, y, x+scaled.Bounds().Dx(), y+scaled.Bounds().Dy())
	imagedraw.Draw(base, rect, scaled, image.Point{}, imagedraw.Over)
	return base
}

func adjustOpacity(img *image.NRGBA, opacity float64) *image.NRGBA {
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = uint8(float64(img.Pix[i]) * opacity)
	}
	return img
}

func encodeToWriter(w io.Writer, img image.Image, ext string, isOpaque bool) error {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		var toEncode image.Image = img
		if !isOpaque {
			toEncode = flattenIfNeeded(img)
		}
		return jpeg.Encode(w, toEncode, &jpeg.Options{Quality: 85})
	case ".png":
		return png.Encode(w, img)
	case ".gif":
		var toEncode image.Image = img
		if !isOpaque {
			toEncode = flattenIfNeeded(img)
		}
		return gif.Encode(w, toEncode, nil)
	case ".tif", ".tiff":
		return xtiff.Encode(w, img, nil)
	case ".webp":
		return webp.Encode(w, img, webp.Options{
			Quality: 85,
			Method:  2,
		})
	default:
		var toEncode image.Image = img
		if !isOpaque {
			toEncode = flattenIfNeeded(img)
		}
		return jpeg.Encode(w, toEncode, &jpeg.Options{Quality: 85})
	}
}

func encodeByExt(img image.Image, ext string) ([]byte, error) {
	var buf bytes.Buffer
	err := encodeToWriter(&buf, img, ext, false)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (p *Processor) encodeAndUpload(ctx context.Context, img image.Image, bucketName, objectName, contentType string, ext string, sourceGeneration string, isOpaque bool, returnBytes bool) ([]byte, error) {
	writer := p.storage.Bucket(bucketName).Object(objectName).NewWriter(ctx)
	writer.ContentType = contentType
	writer.CacheControl = p.cfg.CacheControl
	if sourceGeneration != "" {
		writer.Metadata = map[string]string{
			sourceGenerationMetadataKey: sourceGeneration,
		}
	}

	var buf bytes.Buffer
	var w io.Writer = writer
	if returnBytes {
		w = io.MultiWriter(writer, &buf)
	}

	var err error
	if strings.ToLower(contentType) == "image/webp" {
		err = webp.Encode(w, img, webp.Options{
			Quality: 85,
			Method:  2,
		})
	} else {
		err = encodeToWriter(w, img, ext, isOpaque)
	}

	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("encode image: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close GCS writer: %w", err)
	}

	log.Printf("uploaded gs://%s/%s", bucketName, objectName)
	if returnBytes {
		return buf.Bytes(), nil
	}
	return nil, nil
}

func decodeExif(data []byte) *exifpkg.Exif {
	exifData, err := exifpkg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return exifData
}

func applyEXIFOrientation(img image.Image, exifData *exifpkg.Exif) image.Image {
	if exifData == nil {
		return img
	}
	orientation := exifOrientation(exifData)
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

func exifOrientation(exifData *exifpkg.Exif) int {
	if exifData == nil {
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

func extractAllEXIF(exifData *exifpkg.Exif) map[string]interface{} {
	if exifData == nil {
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
		srcOffset := y * img.Stride
		for x := 0; x < width; x++ {
			dstOffset := (height-1-y)*dst.Stride + (width-1-x)*4
			copy(dst.Pix[dstOffset:dstOffset+4], img.Pix[srcOffset+x*4:srcOffset+x*4+4])
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
		srcOffset := y * img.Stride
		for x := 0; x < width; x++ {
			dstOffset := x*dst.Stride + (height-1-y)*4
			copy(dst.Pix[dstOffset:dstOffset+4], img.Pix[srcOffset+x*4:srcOffset+x*4+4])
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
		srcOffset := y * img.Stride
		for x := 0; x < width; x++ {
			dstOffset := (width-1-x)*dst.Stride + y*4
			copy(dst.Pix[dstOffset:dstOffset+4], img.Pix[srcOffset+x*4:srcOffset+x*4+4])
		}
	}
	return dst
}

func encodeWebP(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	err := webp.Encode(&buf, img, webp.Options{
		Quality: 85,
		Method:  2,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func flattenIfNeeded(img image.Image) image.Image {
	switch typed := img.(type) {
	case *image.NRGBA:
		if !hasAlpha(typed) {
			return typed
		}
		return flattenNRGBA(typed)
	case *image.RGBA:
		if !hasAlphaRGBA(typed) {
			return typed
		}
	}

	nrgba := toNRGBA(img)
	if !hasAlpha(nrgba) {
		return nrgba
	}
	return flattenNRGBA(nrgba)
}

func flattenNRGBA(nrgba *image.NRGBA) *image.RGBA {
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

func hasAlphaRGBA(img *image.RGBA) bool {
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

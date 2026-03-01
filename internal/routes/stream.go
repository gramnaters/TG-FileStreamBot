package routes

import (
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/utils"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gotd/td/tg"
	range_parser "github.com/quantumsheep/range-parser"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
)

var log *zap.Logger

func (e *allRoutes) LoadHome(r *Route) {
	log = e.log.Named("Stream")
	defer log.Info("Loaded stream route")
	r.Engine.GET("/stream/:messageID", getStreamRoute)
}

func getStreamRoute(ctx *gin.Context) {
	w := ctx.Writer
	r := ctx.Request

	messageIDParm := ctx.Param("messageID")
	messageID, err := strconv.Atoi(messageIDParm)
	if err != nil {
		http.Error(w, "invalid message ID", http.StatusBadRequest)
		return
	}

	authHash := ctx.Query("hash")
	if authHash == "" {
		http.Error(w, "missing hash param", http.StatusBadRequest)
		return
	}

	worker := bot.GetNextWorker()
	if worker == nil {
		http.Error(w, "no workers available", http.StatusServiceUnavailable)
		return
	}

	file, err := utils.FileFromMessage(ctx, worker.Client, messageID)
	if err != nil {
		log.Error("Failed to get file from message", zap.Int("messageID", messageID), zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	expectedHash := utils.PackFile(
		file.FileName,
		file.FileSize,
		file.MimeType,
		file.ID,
	)
	if !utils.CheckHash(authHash, expectedHash) {
		http.Error(w, "invalid hash", http.StatusForbidden)
		return
	}

	// Common headers
	ctx.Header("X-Robots-Tag", "noindex, nofollow")
	ctx.Header("Access-Control-Allow-Origin", "*")
	ctx.Header("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")

	// Handle OPTIONS preflight
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// For photo messages: use TelegramReader for proper chunked streaming
	if file.FileSize == 0 {
		// Fetch the first chunk to get actual size, then stream the rest
		res, err := worker.Client.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: file.Location,
			Offset:   0,
			Limit:    1024 * 1024,
		})
		if err != nil {
			log.Error("Failed to fetch photo", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result, ok := res.(*tg.UploadFile)
		if !ok {
			http.Error(w, "unexpected response type from Telegram", http.StatusInternalServerError)
			return
		}
		fileBytes := result.GetBytes()
		ctx.Header("Content-Type", file.MimeType)
		ctx.Header("Content-Length", strconv.Itoa(len(fileBytes)))
		ctx.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))
		ctx.Header("Cache-Control", "public, max-age=3600")
		if r.Method != "HEAD" {
			ctx.Data(http.StatusOK, file.MimeType, fileBytes)
		}
		return
	}

	ctx.Header("Accept-Ranges", "bytes")
	ctx.Header("Cache-Control", "public, max-age=3600")

	var start, end int64
	rangeHeader := r.Header.Get("Range")

	if rangeHeader == "" {
		start = 0
		end = file.FileSize - 1
		w.WriteHeader(http.StatusOK)
	} else {
		ranges, err := range_parser.Parse(file.FileSize, rangeHeader)
		if err != nil {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = ranges[0].Start
		end = ranges[0].End
		ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize))
		log.Debug("Range request", zap.Int64("start", start), zap.Int64("end", end), zap.Int64("fileSize", file.FileSize))
		w.WriteHeader(http.StatusPartialContent)
	}

	contentLength := end - start + 1
	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	disposition := "inline"
	if ctx.Query("d") == "true" {
		disposition = "attachment"
	}

	ctx.Header("Content-Type", mimeType)
	ctx.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	ctx.Header("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"", disposition, file.FileName))

	if r.Method != "HEAD" {
		lr, err := utils.NewTelegramReader(ctx, worker.Client, file.Location, start, end, contentLength)
		if err != nil {
			log.Error("Failed to create Telegram reader", zap.Error(err))
			return
		}
		defer lr.Close()
		written, err := io.CopyN(w, lr, contentLength)
		if err != nil && err != io.EOF {
			log.Warn("Stream copy ended early", zap.Int64("written", written), zap.Int64("expected", contentLength), zap.Error(err))
		}
	}
}

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	videoID, err := uuid.Parse(r.PathValue("videoID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if video.UserID != userID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	videoFileIn, headers, err := r.FormFile("video")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := videoFileIn.Close(); err != nil {
			log.Printf("failed to close file: %v", err)
		}
	}()
	contentType := headers.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if mediaType != "video/mp4" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	videoFileOut, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := os.Remove(videoFileOut.Name()); err != nil {
			log.Printf("failed to delete file: %v", err)
		}
	}()
	defer func() {
		if err := videoFileOut.Close(); err != nil {
			log.Printf("failed to close file: %v", err)
		}
	}()
	if _, err := io.Copy(videoFileOut, videoFileIn); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := videoFileOut.Seek(0, io.SeekStart); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	aspectRatio, err := getVideoAspectRatio(videoFileOut.Name())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	prefix := "other/"
	if aspectRatio == "9:16" {
		prefix = "portrait/"
	} else if aspectRatio == "16:9" {
		prefix = "landscape/"
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	filename := prefix + base64.RawURLEncoding.EncodeToString(buf) + ".mp4"
	params := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(filename),
		Body:        videoFileOut,
		ContentType: aws.String(mediaType),
	}
	if _, err := cfg.s3Client.PutObject(r.Context(), &params); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, filename)
	video.VideoURL = &url
	if err = cfg.db.UpdateVideo(video); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	c := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buf := bytes.Buffer{}
	c.Stdout = &buf
	if err := c.Run(); err != nil {
		return "", err
	}
	data := struct {
		Streams []struct {
			CodecType          string `json:"codec_type"`
			DisplayAspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}{}
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		return "", err
	}
	aspectRatio := "other"
	for _, s := range data.Streams {
		if s.CodecType == "video" {
			if s.DisplayAspectRatio == "9:16" || s.DisplayAspectRatio == "16:9" {
				aspectRatio = s.DisplayAspectRatio
			}
			break
		}
	}
	return aspectRatio, nil
}

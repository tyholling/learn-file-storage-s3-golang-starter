package main

import (
	"bytes"
	"context"
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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (1 << 30))
	videoID, err := uuid.Parse(r.PathValue("videoID"))
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Printf("video id: %s", videoID.String())
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if video.UserID != userID {
		log.Print(err)
		log.Printf("video user id: %q user id: %q", video.UserID, userID)
		log.Printf("%+v", video)
		respondWithError(w, http.StatusUnauthorized, "video user id does not match user id", nil)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	videoFileIn, headers, err := r.FormFile("video")
	if err != nil {
		log.Print(err)
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
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if mediaType != "video/mp4" {
		log.Print(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	videoFileOut, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		log.Print(err)
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
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := videoFileOut.Seek(0, io.SeekStart); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	processedFilePath, err := processVideoForFastStart(videoFileOut.Name())
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := os.Remove(processedFile.Name()); err != nil {
			log.Printf("failed to delete file: %v", err)
		}
	}()
	defer func() {
		if err := processedFile.Close(); err != nil {
			log.Printf("failed to close file: %v", err)
		}
	}()
	aspectRatio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	prefix := "other/"
	switch aspectRatio {
	case "9:16":
		prefix = "portrait/"
	case "16:9":
		prefix = "landscape/"
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	filename := prefix + base64.RawURLEncoding.EncodeToString(buf) + ".mp4"
	params := s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(filename),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	}
	if _, err := cfg.s3Client.PutObject(r.Context(), &params); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	video.VideoURL = aws.String(cfg.s3Bucket + "," + filename)

	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = cfg.db.UpdateVideo(signedVideo); err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	respondWithJSON(w, http.StatusOK, signedVideo)
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

func processVideoForFastStart(filePath string) (string, error) {
	tempPath := filePath + ".processed"
	c := exec.Command(
		"ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", tempPath)
	buf := bytes.Buffer{}
	c.Stdout = &buf
	if err := c.Run(); err != nil {
		return "", err
	}
	return tempPath, nil
}

func generatePresignedURL(c *s3.Client, bucket, key string, exp time.Duration) (string, error) {
	client := s3.NewPresignClient(c)

	params := s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	r, err := client.PresignGetObject(context.Background(), &params, s3.WithPresignExpires(exp))
	if err != nil {
		log.Print(err)
		return "", err
	}
	// log.Printf("presigned url: %s", r.URL)
	return r.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	videoURL := aws.ToString(video.VideoURL)
	// if videoURL == "" {
	// 	log.Print("error: video url is empty")
	// 	return video, fmt.Errorf("video url is empty")
	// }
	tokens := strings.Split(videoURL, ",")
	// log.Printf("url: %s tokens: %v", videoURL, tokens)
	if len(tokens) != 2 {
		return video, fmt.Errorf("failed to parse video url: %s", videoURL)
	}
	bucket := tokens[0]
	key := tokens[1]
	// log.Printf("bucket: %q key: %q", bucket, key)

	url, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Hour)
	if err != nil {
		log.Print(err)
		return video, err
	}
	video.VideoURL = aws.String(url)
	return video, nil
}

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoID, err := uuid.Parse(r.PathValue("videoID"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}
	log.Printf("video id: %s", videoID.String())

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	if err = r.ParseMultipartForm(10 << 20); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	thumbnailFileIn, headers, err := r.FormFile("thumbnail")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	contentType := headers.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusBadRequest, "invalid file type: "+mediaType, nil)
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

	tokens := strings.Split(contentType, "/")
	if len(tokens) != 2 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	extension := tokens[1]

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	filename := base64.RawURLEncoding.EncodeToString(buf) + "." + extension
	path := filepath.Join(cfg.assetsRoot, filename)
	thumbnailFileOut, err := os.Create(path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(thumbnailFileOut, thumbnailFileIn); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	thumbnailURL := fmt.Sprintf("http://192.168.64.2:8091/%s", path)
	video.ThumbnailURL = &thumbnailURL

	if err = cfg.db.UpdateVideo(video); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

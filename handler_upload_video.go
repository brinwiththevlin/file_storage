package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	var limit int64 = 1 << 30
	reader := http.MaxBytesReader(w, r.Body, limit)
	r.Body = reader

	videoID, err := uuid.Parse(r.PathValue("videoID"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

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

	videoMeta, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't fetch video", err)
		return
	}
	if videoMeta.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Couldn't fetch video", err)
		return
	}

	videoFile, videoHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Request has no video key", err)
		return
	}
	defer videoFile.Close()
	contentType := videoHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Content-Type must be video/mp4", nil)
		return
	}

	localVideo, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not process video", err)
		return
	}

	defer os.Remove(localVideo.Name())
	defer localVideo.Close()

	_, err = io.Copy(localVideo, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not process video", err)
		return
	}

	localVideo.Seek(0, io.SeekStart)

	bytes := make([]byte, 32)
	_, err = rand.Read(bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not generate temproary video name", err)
	}
	videoName := hex.EncodeToString(bytes) + ".mp4"
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &videoName, Body: localVideo, ContentType: &contentType})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not upload video", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoName)
	videoMeta.VideoURL = &videoURL
	cfg.db.UpdateVideo(videoMeta)

	respondWithJSON(w, http.StatusOK, struct{}{})
}

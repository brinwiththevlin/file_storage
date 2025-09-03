package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"

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

	ratio, err := getVideoAspectRatio(localVideo.Name())
	var prefix string
	switch ratio {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	default:
		prefix = "other/"
	}

	bytes := make([]byte, 32)
	_, err = rand.Read(bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not generate temproary video name", err)
	}
	videoName := prefix + hex.EncodeToString(bytes) + ".mp4"
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

func getVideoAspectRatio(filePath string) (string, error) {
	type Stream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type VideoMeta struct {
		Streams []Stream `json:"streams"`
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams", filePath,
	)
	var b bytes.Buffer
	cmd.Stdout = &b

	err := cmd.Run()
	if err != nil {
		log.Println("ffprobe failed", err)
		return "", err
	}

	meta := VideoMeta{}
	err = json.Unmarshal(b.Bytes(), &meta)
	if err != nil {
		log.Println(err)
		return "", err
	}

	const sixteenNine float64 = 16.0 / 9.0
	ratio := float64(meta.Streams[0].Width) / float64(meta.Streams[0].Height)

	tolerance := 0.4
	if ratio < sixteenNine+tolerance && ratio > sixteenNine-tolerance {
		return "16:9", nil
	}
	if ratio < 1/sixteenNine+tolerance && ratio > 1/sixteenNine-tolerance {
		return "9:16", nil
	}
	return "other", nil
}

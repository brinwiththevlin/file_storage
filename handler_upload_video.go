package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
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
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not get aspect ratio", err)
		return
	}
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

	// videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, videoName)
	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, videoName)
	videoMeta.VideoURL = &videoURL
	cfg.db.UpdateVideo(videoMeta)
	signed, err := cfg.dbVideoToSignedVideo(videoMeta)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not sign video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signed)
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

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	preClient := s3.NewPresignClient(s3Client)
	obj, err := preClient.PresignGetObject(context.Background(), &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(expireTime))
	if err != nil {
		log.Println("could not get presign object", err)
		return "", err
	}

	return obj.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return database.Video{}, errors.New("bad bucket-key string")
	}

	log.Println(*cfg.s3Client)
	signedURL, err := generatePresignedURL(cfg.s3Client, parts[0], parts[1], time.Hour)
	if err != nil {
		return database.Video{}, err
	}
	video.VideoURL = &signedURL
	return video, nil
}

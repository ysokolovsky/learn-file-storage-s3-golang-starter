package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	defer r.Body.Close()

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
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

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video metadata", err)
		return
	}

	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", errors.New("unauthorized"))
		return
	}

	const maxMemory = 1 << 30

	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "file is not a video", err)
		return
	}

	tempFile, _ := os.CreateTemp("", "tubely-upload.mp4")
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	io.Copy(tempFile, file)

	tempFile.Seek(0, io.SeekStart)

	dar, _ := getVideoAspectRatio(tempFile.Name())

	key := make([]byte, 32)
	rand.Read(key)
	s3key := base64.RawURLEncoding.EncodeToString(key)

	if dar == "16:9" {
		s3key = "landscape/" + s3key
	} else if dar == "9:16" {
		s3key = "portrait/" + s3key
	} else {
		s3key = "other/" + s3key
	}

	processedVideoPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "processing error", err)
	}

	processedVideo, _ := os.Open(processedVideoPath)
	defer os.Remove(processedVideo.Name())
	defer processedVideo.Close()

	poi := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3key,
		Body:        processedVideo,
		ContentType: &mediaType,
	}

	_, err = cfg.s3Client.PutObject(context.TODO(), &poi)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "s3 upload error", err)
	}

	locator := cfg.s3Bucket + "," + s3key
	videoMetadata.VideoURL = &locator

	if err = cfg.db.UpdateVideo(videoMetadata); err != nil {
		respondWithError(w, http.StatusInternalServerError, "db update error", err)
		return
	}

	signedVideo, err := cfg.dbVideoToSignedVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}

type FFProbe struct {
	Streams []Stream `json:"streams"`
}

type Stream struct {
	Index              int    `json:"index"`
	CodecType          string `json:"codec_type"`
	DisplayAspectRatio string `json:"display_aspect_ratio"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	out := bytes.Buffer{}
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var jsonRes map[string]any

	json.Unmarshal(out.Bytes(), &jsonRes)

	var probe FFProbe
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return "", err
	}
	var dar string
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			dar = s.DisplayAspectRatio
			break
		}
	}

	if dar == "16:9" || dar == "9:16" {
		return dar, nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFile := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFile)
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	return outputFile, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	presignedHTTPRequest, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}
	return presignedHTTPRequest.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return database.Video{}, errors.New("no url")
	}
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return database.Video{}, fmt.Errorf("invalid video url format (expected bucket,key): %q", *video.VideoURL)
	}

	bucket := parts[0]
	key := parts[1]

	presigned, err := generatePresignedURL(cfg.s3Client, bucket, key, 10*time.Minute)
	video.VideoURL = &presigned
	return video, err
}

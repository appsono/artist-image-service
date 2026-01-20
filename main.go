package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

/// CachedImage represents metadata about a cached artist image
type CachedImage struct {
	ArtistName string    `json:"artist_name"`
	ImageKey   string    `json:"image_key"`
	URL        string    `json:"url"`
	Source     string    `json:"source"`
	FetchedAt  time.Time `json:"fetched_at"`
}

/// APIResponse is the JSON response structure
type APIResponse struct {
	Success    bool      `json:"success"`
	ImageURL   string    `json:"image_url,omitempty"`
	Source     string    `json:"source,omitempty"`
	CachedAt   time.Time `json:"cached_at,omitempty"`
	ArtistName string    `json:"artist_name,omitempty"`
	Error      string    `json:"error,omitempty"`
}

/// ArtistImageService handles artist image operations
type ArtistImageService struct {
	db          *sql.DB
	minioClient *minio.Client
	bucket      string
}

/// NewArtistImageService creates a new service instance
func NewArtistImageService(dbPath, minioEndpoint, accessKey, secretKey, bucket string, useSSL bool) (*ArtistImageService, error) {
	//Initialize SQLite database
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	//Create table if not exists
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS artist_images (
			artist_name_lower TEXT PRIMARY KEY,
			artist_name TEXT NOT NULL,
			image_key TEXT NOT NULL,
			url TEXT NOT NULL,
			source TEXT NOT NULL,
			fetched_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	//Initialize MinIO client
	minioClient, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	//Create bucket if not exists
	ctx := context.Background()
	exists, err := minioClient.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}

	if !exists {
		err = minioClient.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create bucket: %w", err)
		}
		log.Printf("Created MinIO bucket: %s\n", bucket)
	}

	//Set bucket to public read policy
	policy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": {"AWS": ["*"]},
			"Action": ["s3:GetObject"],
			"Resource": ["arn:aws:s3:::%s/*"]
		}]
	}`, bucket)

	err = minioClient.SetBucketPolicy(ctx, bucket, policy)
	if err != nil {
		log.Printf("Warning: failed to set bucket policy: %v\n", err)
	}

	service := &ArtistImageService{
		db:          db,
		minioClient: minioClient,
		bucket:      bucket,
	}

	//Load existing cache count
	count, _ := service.getCacheCount()
	log.Printf("Loaded %d cached artist images from database\n", count)

	return service, nil
}

/// Close closes the database connection
func (s *ArtistImageService) Close() error {
	return s.db.Close()
}

/// getCacheCount returns the number of cached images
func (s *ArtistImageService) getCacheCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM artist_images").Scan(&count)
	return count, err
}

/// getCachedImage retrieves cached image metadata from database
func (s *ArtistImageService) getCachedImage(artistName string) (*CachedImage, error) {
	cacheKey := strings.ToLower(strings.TrimSpace(artistName))

	var cached CachedImage
	var fetchedAtUnix int64

	err := s.db.QueryRow(`
		SELECT artist_name, image_key, url, source, fetched_at
		FROM artist_images
		WHERE artist_name_lower = ?
	`, cacheKey).Scan(&cached.ArtistName, &cached.ImageKey, &cached.URL, &cached.Source, &fetchedAtUnix)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	cached.FetchedAt = time.Unix(fetchedAtUnix, 0)
	return &cached, nil
}

/// saveCachedImage saves image metadata to database
func (s *ArtistImageService) saveCachedImage(cached *CachedImage) error {
	cacheKey := strings.ToLower(strings.TrimSpace(cached.ArtistName))

	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO artist_images (artist_name_lower, artist_name, image_key, url, source, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, cacheKey, cached.ArtistName, cached.ImageKey, cached.URL, cached.Source, cached.FetchedAt.Unix())

	return err
}

/// scrapeLastFmImage scrapes artist image
func (s *ArtistImageService) scrapeLastFmImage(artistName string) (string, error) {
	//URL encode artist name
	encodedName := strings.ReplaceAll(artistName, " ", "+")
	lastfmURL := fmt.Sprintf("https://www.last.fm/music/%s", encodedName)

	log.Printf("Scraping Last.fm: %s\n", lastfmURL)

	//Make HTTP request with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", lastfmURL, nil)
	if err != nil {
		return "", err
	}

	//Set headers
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/115.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Referer", "https://www.google.com/")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to fetch Last.fm page: status %d", resp.StatusCode)
	}

	//Parse HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	/// Try multiple selectors to find the artist image
	var imageURL string

	//Try header image first
	doc.Find("img.header-new-background-image").Each(func(i int, s *goquery.Selection) {
		if src, exists := s.Attr("src"); exists && imageURL == "" {
			imageURL = src
		}
	})

	//Try artist avatar
	if imageURL == "" {
		doc.Find("img.avatar").Each(func(i int, s *goquery.Selection) {
			if src, exists := s.Attr("src"); exists {
				imageURL = src
			}
		})
	}

	//Try meta tags
	if imageURL == "" {
		doc.Find("meta[property='og:image']").Each(func(i int, s *goquery.Selection) {
			if content, exists := s.Attr("content"); exists {
				imageURL = content
			}
		})
	}

	if imageURL == "" {
		return "", fmt.Errorf("no image found on Last.fm page")
	}

	//Filter out placeholder images
	if strings.Contains(imageURL, "2a96cbd8b46e442fc41c2b86b821562f") ||
		strings.Contains(imageURL, "c6f59c1e5e7240a4c0d427abd71f3dbb") {
		return "", fmt.Errorf("only placeholder image available")
	}

	log.Printf("Found image: %s\n", imageURL)
	return imageURL, nil
}

/// uploadImageToMinio downloads and uploads image to MinIO
func (s *ArtistImageService) uploadImageToMinio(imageURL, artistName string) (string, error) {
	//Download image
	resp, err := http.Get(imageURL)
	if err != nil {
		return "", fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to download image: status %d", resp.StatusCode)
	}

	//Read image data
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read image data: %w", err)
	}

	//Determine content type and file extension
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/jpeg"
	}

	ext := ".jpg"
	if strings.Contains(contentType, "png") {
		ext = ".png"
	} else if strings.Contains(contentType, "webp") {
		ext = ".webp"
	}

	//Create safe filename (use timestamp to ensure uniqueness)
	safeArtistName := strings.ReplaceAll(artistName, "/", "_")
	safeArtistName = strings.ReplaceAll(safeArtistName, "\\", "_")
	imageKey := fmt.Sprintf("%s_%d%s", safeArtistName, time.Now().Unix(), ext)

	//Upload to MinIO
	ctx := context.Background()
	_, err = s.minioClient.PutObject(ctx, s.bucket, imageKey, bytes.NewReader(imageData), int64(len(imageData)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to MinIO: %w", err)
	}

	log.Printf("Uploaded image to MinIO: %s\n", imageKey)
	return imageKey, nil
}

/// getMinioURL generates the public URL for a MinIO object
func (s *ArtistImageService) getMinioURL(imageKey string) string {
	endpoint := os.Getenv("MINIO_PUBLIC_ENDPOINT")

	useSSL := os.Getenv("MINIO_USE_SSL") == "true"
	protocol := "http"
	if useSSL {
		protocol = "https"
	}

	return fmt.Sprintf("%s://%s/%s/%s", protocol, endpoint, s.bucket, imageKey)
}

/// GetArtistImage fetches or retrieves cached artist image
func (s *ArtistImageService) GetArtistImage(artistName string) (*CachedImage, error) {
	//Check cache first
	cached, err := s.getCachedImage(artistName)
	if err != nil {
		log.Printf("Warning: failed to check cache: %v\n", err)
	}

	//If cached and fresh => return it
	if cached != nil && time.Since(cached.FetchedAt) < 7*24*time.Hour {
		log.Printf("Returning cached image for: %s\n", artistName)
		return cached, nil
	}

	//Not in cache or stale => fetch new image
	log.Printf("Fetching new image for: %s\n", artistName)

	imageURL, err := s.scrapeLastFmImage(artistName)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape image: %w", err)
	}
	
	imageKey, err := s.uploadImageToMinio(imageURL, artistName)
	if err != nil {
		//If upload fails => still cache the original URL
		log.Printf("Warning: failed to upload to MinIO, caching original URL: %v\n", err)
		imageKey = ""
	}

	finalURL := imageURL
	if imageKey != "" {
		finalURL = s.getMinioURL(imageKey)
	}

	//Cache the result
	cached = &CachedImage{
		ArtistName: artistName,
		ImageKey:   imageKey,
		URL:        finalURL,
		Source:     "last.fm",
		FetchedAt:  time.Now(),
	}

	err = s.saveCachedImage(cached)
	if err != nil {
		log.Printf("Warning: failed to save cache: %v\n", err)
	}

	return cached, nil
}

/// HTTP Handlers
func (s *ArtistImageService) handleGetArtistImage(w http.ResponseWriter, r *http.Request) {
	artistName := r.URL.Query().Get("name")
	if artistName == "" {
		s.sendJSONResponse(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "artist name is required",
		})
		return
	}

	cached, err := s.GetArtistImage(artistName)
	if err != nil {
		s.sendJSONResponse(w, http.StatusNotFound, APIResponse{
			Success:    false,
			Error:      err.Error(),
			ArtistName: artistName,
		})
		return
	}

	s.sendJSONResponse(w, http.StatusOK, APIResponse{
		Success:    true,
		ImageURL:   cached.URL,
		Source:     cached.Source,
		CachedAt:   cached.FetchedAt,
		ArtistName: cached.ArtistName,
	})
}

func (s *ArtistImageService) handleServeImage(w http.ResponseWriter, r *http.Request) {
	artistName := r.URL.Query().Get("name")
	if artistName == "" {
		http.Error(w, "artist name is required", http.StatusBadRequest)
		return
	}

	cached, err := s.GetArtistImage(artistName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	//Redirect to the MinIO URL or original URL
	http.Redirect(w, r, cached.URL, http.StatusFound)
}

func (s *ArtistImageService) handleStats(w http.ResponseWriter, r *http.Request) {
	count, err := s.getCacheCount()
	if err != nil {
		log.Printf("Error getting cache count: %v\n", err)
		count = 0
	}

	stats := map[string]interface{}{
		"cached_artists": count,
		"bucket":         s.bucket,
		"storage":        "minio",
		"database":       "sqlite",
	}

	s.sendJSONResponse(w, http.StatusOK, stats)
}

func (s *ArtistImageService) sendJSONResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func main() {
	//Load environment variables
	godotenv.Load()
	port := os.Getenv("PORT")
	dbPath := os.Getenv("DB_PATH")
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
	minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
	minioBucket := os.Getenv("MINIO_BUCKET")
	minioUseSSL := os.Getenv("MINIO_USE_SSL") == "true"

	//Create service
	service, err := NewArtistImageService(dbPath, minioEndpoint, minioAccessKey, minioSecretKey, minioBucket, minioUseSSL)
	if err != nil {
		log.Fatalf("Failed to create service: %v\n", err)
	}
	defer service.Close()

	//Setup router
	router := mux.NewRouter()

	//API endpoints
	router.HandleFunc("/api/artist-image", service.handleGetArtistImage).Methods("GET")
	router.HandleFunc("/api/artist-image/serve", service.handleServeImage).Methods("GET")
	router.HandleFunc("/api/stats", service.handleStats).Methods("GET")

	//Health check
	router.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	//CORS middleware
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	})

	//Start server
	log.Printf("Starting Artist Image Service on port %s\n", port)
	log.Printf("Database: %s\n", dbPath)
	log.Printf("MinIO: %s (bucket: %s)\n", minioEndpoint, minioBucket)
	log.Fatal(http.ListenAndServe(":"+port, router))
}
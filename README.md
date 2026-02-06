# Artist Profile Picture Service

A Go microservice that scrapes and serves artist profile pictures from Deezer. Uses MinIO for object storage and SQLite for metadata management.

This service is used in Sono to fetch and cache artist profile pictures.

## Architecture

**Storage Layer:**
- **MinIO**: S3-compatible object storage for images
- **SQLite**: Lightweight database for image metadata and cache management

## Features

- Scrapes artist images from Last.fm
- Docker Compose deployment with MinIO included
- Automatic cache refresh (7-day TTL)
- Filters out placeholder images

## API Endpoints

### Get Artist Image Info
```
GET /api/artist-image?name={artistName}
```

Returns JSON with image URL and metadata:
```json
{
  "success": true,
  "image_url": "http://localhost:9000/artist-images/Radiohead_1737403200.jpg",
  "source": "last.fm",
  "cached_at": "2026-01-20T10:30:00Z",
  "artist_name": "Radiohead"
}
```

### Serve Artist Image
```
GET /api/artist-image/serve?name={artistName}
```

Redirects to the MinIO URL where the image is stored.

### Stats
```
GET /api/stats
```

Returns cache statistics:
```json
{
  "cached_artists": 42,
  "bucket": "artist-images",
  "storage": "minio",
  "database": "sqlite"
}
```

### Health Check
```
GET /health
```

Returns `OK` if service is running.

## Quick Start

### Using Docker Compose (Recommended)

1. Build and run all services:
```bash
docker-compose up -d
```

This starts:
- MinIO on port 9001 (storage)
- MinIO Console on port 9004 (web UI)
- Artist Image Service on port 8080 (API)

2. Test it:
```bash
curl "http://localhost:8080/api/artist-image?name=Radiohead"
```

3. Access MinIO Console:
```
http://localhost:9004
Username: minioadmin
Password: minioadmin
```

### Local Development

If you want to run outside Docker:

1. Start MinIO locally:
```bash
docker run -p 9003:9000 -p 9004:9001 \
  -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio:RELEASE.2023-09-04T19-57-37Z \
  server /data --console-address ":9001"
```

2. Install Go dependencies:
```bash
go mod download
```

3. Run the service:
```bash
go run main.go
```

## Configuration

Create a `.env` file:
```env
PORT=8080

# MinIO Configuration
MINIO_ENDPOINT=localhost:9003
MINIO_ACCESS_KEY=minioadmin
MINIO_SECRET_KEY=minioadmin
MINIO_BUCKET=artist-images
MINIO_USE_SSL=false

# Database Configuration
DB_PATH=./data/cache.db
```

## Docker Deployment

The `docker-compose.yml` includes:
- **MinIO service**: Object storage with persistent volume
- **Artist Image Service**: Go API with CGO-enabled SQLite support
- Health checks for both services
- Automatic restart policies
- Volume mounting for data persistence

## How It Works

1. Client requests artist image by name
2. Service checks SQLite database for cached metadata
3. If cached and fresh (<7 days), returns MinIO URL
4. If not cached or stale:
   - Scrapes Deezer artist page for image
   - Downloads image
   - Uploads to MinIO bucket
   - Saves metadata to SQLite
   - Returns MinIO URL
5. Images are served directly from MinIO (S3-compatible)

## Data Persistence

- **MinIO data**: Stored in `./minio-data/` directory
- **SQLite database**: Stored in `./data/cache.db`
- **Both persist** across container restarts via Docker volumes

## Cache Management

- Images cached for 7 days (configurable)
- Metadata stored in SQLite with indexed lookups
- Automatic cleanup of stale cache entries
- Atomic database operations prevent corruption

## Troubleshooting

**MinIO connection failed:**
- Check MinIO is running: `docker ps | grep minio`
- Verify MinIO port: `curl http://localhost:9003/minio/health/live`

**SQLite database locked:**
- SQLite handles this automatically with retries
- If persists, check file permissions on `./data/`

**Images not appearing:**
- Check MinIO bucket policy is set to public read
- Verify images exist: `docker exec -it artist-image-minio ls /data/artist-images/`

## License

MIT
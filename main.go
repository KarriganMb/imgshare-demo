package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
)

var dbConn *pgx.Conn

func main() {
	fmt.Println("Formatted time (RFC3339):", time.Now().Format(time.RFC3339))

	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	connString := "postgres://postgres:postgress@localhost:5432/postgres?sslmode=disable"
	dbConn, err = pgx.Connect(context.Background(), connString)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}
	defer dbConn.Close(context.Background())
	fmt.Println("Database connected!")

	var result int
	err = dbConn.QueryRow(context.Background(), "SELECT 1").Scan(&result)
	if err != nil {
		log.Fatal("Database query failed:", err)
	}
	fmt.Println("Database test query successful!")

	//createTestData() //DEBUG
	//cleanUp()
	c := cron.New()
	_, err = c.AddFunc("0 12 * * *", func() {
		fmt.Println("Cleaning expired data")
		if err := cleanUp(); err != nil {
			log.Println("cleanup failed", err)
			return
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	c.Start()
	defer c.Stop()
	r := chi.NewRouter()

	r.Get("/", homeHandler)
	r.Post("/upload", uploadHandler)
	r.Get("/{slug:[a-zA-Z0-9]+}", resolveImg)

	fmt.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Serve upload form
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	// Step 1: Parse form (max 10MB)
	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		fmt.Println("ParseMultipartForm error:", err) // Add this
		http.Error(w, "File too large", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "File not found", http.StatusBadRequest)
		return
	}

	defer file.Close()

	valid, bfile, contentType, err := isValidImage(file)
	fmt.Println("Valid:", valid, "ContentType:", contentType, "Error:", err) // DEBUG

	if err != nil {
		fmt.Println("Returning due to err")
		http.Error(w, "File not valid", http.StatusBadRequest)
		return
	}
	if !valid {
		fmt.Println("Returning due to !valid")
		http.Error(w, "File not valid", http.StatusBadRequest)
		return
	}
	fmt.Println("Validation passed!")     //DEBUG
	fmt.Println("About to generate slug") //DEBUG

	slug := generateSlug()
	fmt.Println("Generated slug:", slug) //DEBUG
	url := "http://localhost:8000/storage/v1/object/images/" + slug

	ffile := bytes.NewReader(bfile)

	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")

	req, _ := http.NewRequest("POST", url, ffile)
	req.Header.Set("Authorization", "Bearer "+serviceKey)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{}
	resp, _ := client.Do(req)
	fmt.Println("Storage response status:", resp.StatusCode) //DEBUG
	if resp.StatusCode != 200 {
		fmt.Println("Storage upload failed!") //DEBUG
		http.Error(w, "Storage upload failed", http.StatusBadRequest)
		return
	}
	fmt.Println("Storage upload succeeded!") //DEBUG

	_, err = dbConn.Exec(
		context.Background(),
		"INSERT INTO images (slug, expires_at) VALUES ($1, NOW() + INTERVAL '10 days')",
		slug,
	)
	if err != nil {
		fmt.Println("Database insert failed:", err) //DEBUG
		http.Error(w, "Database query failed", http.StatusInternalServerError)
		deleteImg(slug)
		return
	}
	fmt.Println("Database insert succeeded!") //DEBUG
	fmt.Println("Returning success to user")  //DEBUG
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(slug))
}

func generateSlug() string {
	const str = "abcdefghijklmnopqrstuvwxyz0123456789"
	const length = 8

	result := make([]byte, length)

	for i := 0; i < length; i++ {
		result[i] = str[rand.Intn(len(str))]
	}
	return string(result)
}

func isValidImage(file io.Reader) (bool, []byte, string, error) {
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		return false, nil, "", err
	}

	// Step 2: Check if file is too small (less than 4 bytes can't be an image)
	if len(fileBytes) < 4 {
		return false, nil, "", errors.New("file is too short")
	}

	jpegM := []byte{255, 216, 255}
	pngM := []byte{137, 80, 78, 71}
	gifM := []byte{71, 73, 70, 56}
	webpM := []byte{82, 73, 70, 70}

	isJPEG := bytes.Equal(fileBytes[0:3], jpegM)
	isPNG := bytes.Equal(fileBytes[0:4], pngM)
	isGIF := bytes.Equal(fileBytes[0:4], gifM)
	isWEBP := bytes.Equal(fileBytes[0:4], webpM)

	if isJPEG || isPNG || isGIF || isWEBP {
		if isJPEG {
			return true, fileBytes, "image/jpeg", nil
		}
		if isPNG {
			return true, fileBytes, "image/png", nil
		}
		if isGIF {
			return true, fileBytes, "image/gif", nil
		}
		if isWEBP {
			return true, fileBytes, "image/webp", nil
		}
	}

	return false, nil, "", errors.New("file is not an image")
}

func deleteImg(slug string) error {
	url := "http://localhost:8000/storage/v1/object/images/" + slug

	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")

	req, _ := http.NewRequest("DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+serviceKey)

	client := &http.Client{}
	resp, _ := client.Do(req)
	if resp.StatusCode != 200 {
		return errors.New("Failed to delete image " + resp.Status)
	}
	return nil
}

func cleanUp() error {
	fmt.Println("Cleaning up", time.Now().Format(time.RFC3339))

	rows, err := dbConn.Query(
		context.Background(),
		"SELECT slug FROM images WHERE expires_at < NOW()",
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var slugs []string
	for rows.Next() {
		var slug string
		err = rows.Scan(&slug)
		if err := rows.Scan(&slug); err != nil {
			return err
		}
		slugs = append(slugs, slug)
	}

	for _, slug := range slugs {
		fmt.Println("DELETED:", slug)

		if err := deleteImg(slug); err != nil {
			return err
		}

		_, err = dbConn.Exec(
			context.Background(),
			"DELETE FROM images WHERE slug = $1", slug,
		)
	}
	fmt.Println("Cleaned up", len(slugs), "images")
	return nil
}

func createTestData() {
	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")
	fmt.Println("Service key length:", len(serviceKey)) //DEBUG
	if len(serviceKey) == 0 {
		fmt.Println("ERROR: SERVICE_ROLE_KEY is empty!")
		return
	}

	testImages := []struct {
		slug string
		url  string
		days int
	}{
		{"test0001", "https://avatars.githubusercontent.com/u/54469796?s=200&v=4", 1},
		{"test0002", "https://avatars.githubusercontent.com/u/54469796?s=200&v=4", 2},
		{"test0003", "https://avatars.githubusercontent.com/u/54469796?s=200&v=4", 3},
		{"test0004", "https://avatars.githubusercontent.com/u/54469796?s=200&v=4", 5},
		{"test0005", "https://avatars.githubusercontent.com/u/54469796?s=200&v=4", 10},
	}

	for _, img := range testImages {
		fmt.Println("Creating test image:", img.slug)

		// Step 1: Download the image
		resp, err := http.Get(img.url)
		if err != nil {
			fmt.Println("Failed to download:", err)
			continue
		}
		imageData, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// Step 2: Upload to Supabase Storage
		storageURL := "http://localhost:8000/storage/v1/object/images/" + img.slug
		req, _ := http.NewRequest("POST", storageURL, bytes.NewReader(imageData))
		req.Header.Set("Authorization", "Bearer "+serviceKey)
		req.Header.Set("Content-Type", "image/png")

		client := &http.Client{}
		storageResp, err := client.Do(req)
		if err != nil {
			fmt.Println("HTTP request error for", img.slug, ":", err)
			continue
		}
		fmt.Println("Storage response status for", img.slug, ":", storageResp.StatusCode) //DEBUG
		if storageResp.StatusCode != 200 {
			body, _ := io.ReadAll(storageResp.Body)
			fmt.Println("Storage upload failed for", img.slug, "- Status:", storageResp.StatusCode, "Body:", string(body))
			storageResp.Body.Close()
			continue
		}

		// Step 3: Insert into database with expired date
		expiresAt := time.Now().AddDate(0, 0, -img.days) // X days ago
		_, err = dbConn.Exec(
			context.Background(),
			"INSERT INTO images (slug, expires_at) VALUES ($1, $2)",
			img.slug,
			expiresAt,
		)
		if err != nil {
			fmt.Println("Database insert failed:", err)
			continue
		}

		fmt.Println("✓ Created:", img.slug, "expired", img.days, "days ago")
	}

	fmt.Println("Test data creation complete!")
}

func resolveImg(w http.ResponseWriter, r *http.Request) {
	//w.Header().Set("Content-Type", "image/png")
	slug := chi.URLParam(r, "slug")
	if len(slug) < 8 || len(slug) > 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Bad Slug"))
		return
	}

	if err := getImage(slug); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("No such image"))
		return
	}

	url := "http://localhost:8000/storage/v1/object/images/" + slug

	serviceKey := os.Getenv("SUPABASE_SERVICE_KEY")

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	req.Header.Set("Authorization", "Bearer "+serviceKey)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))

	if _, err := io.Copy(w, resp.Body); err != nil {
		http.Error(w, "Failed to send image", http.StatusInternalServerError)
		return
	}
}

func getImage(slug string) error {
	query := "SELECT slug FROM images WHERE slug = $1 AND expires_at > NOW() LIMIT 1"
	err := dbConn.QueryRow(context.Background(), query, slug).Scan(&slug)
	if err != nil {
		fmt.Println("Database query failed:", err)
		return err
	}
	return nil
}

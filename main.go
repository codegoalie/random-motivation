package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/codegoalie/random-motivation/db"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// queueEntry pairs a motivation's database id with its text so the queue
// can locate and remove a specific motivation.
type queueEntry struct {
	id   int64
	text string
}

// MotivationQueue holds a shuffled list of motivations and the current position
type MotivationQueue struct {
	motivations []queueEntry
	currentPos  int
	mu          sync.Mutex
}

// Next returns the next motivation in the queue, cycling back to the start when done
func (mq *MotivationQueue) Next() (string, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	if len(mq.motivations) == 0 {
		return "", echo.NewHTTPError(http.StatusNotFound, "no motivations found")
	}

	motivation := mq.motivations[mq.currentPos]
	mq.currentPos = (mq.currentPos + 1) % len(mq.motivations)

	return motivation.text, nil
}

// Add appends a motivation to the end of the queue.
func (mq *MotivationQueue) Add(id int64, text string) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	mq.motivations = append(mq.motivations, queueEntry{id: id, text: text})
}

// Remove deletes the entry with the given id from the queue, keeping
// currentPos valid. It returns whether an entry was removed.
func (mq *MotivationQueue) Remove(id int64) bool {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	index := -1
	for i, entry := range mq.motivations {
		if entry.id == id {
			index = i
			break
		}
	}
	if index == -1 {
		return false
	}

	mq.motivations = append(mq.motivations[:index], mq.motivations[index+1:]...)

	if index < mq.currentPos {
		mq.currentPos--
	}

	if len(mq.motivations) == 0 {
		mq.currentPos = 0
	} else {
		mq.currentPos = ((mq.currentPos % len(mq.motivations)) + len(mq.motivations)) % len(mq.motivations)
	}

	return true
}

// NewMotivationQueue creates a new queue from a list of motivations and shuffles them
func NewMotivationQueue(motivations []db.Motivation) *MotivationQueue {
	entries := make([]queueEntry, len(motivations))
	for i, m := range motivations {
		entries[i] = queueEntry{id: m.ID, text: m.Text}
	}

	// Shuffle the list
	rand.Shuffle(len(entries), func(i, j int) {
		entries[i], entries[j] = entries[j], entries[i]
	})

	return &MotivationQueue{
		motivations: entries,
		currentPos:  0,
	}
}

// GetRenderServiceURL returns the render service URL from environment or default
func GetRenderServiceURL() string {
	if url := os.Getenv("RENDER_SERVICE_URL"); url != "" {
		return url
	}
	return "http://localhost:8081/render"
}

func main() {
	// Initialize database
	database, err := db.New(db.GetDBPath())
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer func() {
		err := database.Close()
		if err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	motivations, err := database.GetAll()
	if err != nil {
		log.Fatalf("Failed to retrieve motivations: %v", err)
	}
	slog.Info("Motivations in database:", "count", len(motivations))
	for _, m := range motivations {
		log.Printf(" - [%d] %s (created at %s)", m.ID, m.Text, m.CreatedAt)
	}

	// Create and shuffle the motivation queue
	queue := NewMotivationQueue(motivations)
	slog.Info("Shuffled motivations queue initialized", "count", len(motivations))

	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Store database and queue in context
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("db", database)
			c.Set("queue", queue)
			return next(c)
		}
	})

	// Routes
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Welcome to the Random Motivation API!\n\n"+
			"Endpoints:\n"+
			"GET /motivation - Get a random motivation\n"+
			"POST /motivation - Add a new motivation (send motivation text in request body)\n"+
			"GET /motivations - List all motivations\n"+
			"DELETE /motivation/:id - Delete a motivation by id\n"+
			"GET /motivations.png - Get a random motivation as an image")
	})
	e.GET("/motivation", getMotivation)
	e.POST("/motivation", postMotivation)
	e.GET("/motivations", listMotivations)
	e.DELETE("/motivation/:id", deleteMotivation)
	e.GET("/motivations.png", getMotivationPNG)

	// Graceful shutdown
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		<-sigint
		log.Println("Shutting down server...")
		if err := e.Close(); err != nil {
			log.Printf("Error closing server: %v", err)
		}
	}()

	// Start server
	e.Logger.Fatal(e.Start(":8080"))
}

// getMotivation returns the next motivation from the shuffled queue
func getMotivation(c echo.Context) error {
	queue := c.Get("queue").(*MotivationQueue)

	motivation, err := queue.Next()
	if err != nil {
		if strings.Contains(err.Error(), "no motivations found") {
			return c.String(http.StatusNotFound, "No motivations found")
		}
		return c.String(http.StatusInternalServerError, "Error retrieving motivation")
	}

	return c.String(http.StatusOK, motivation)
}

// postMotivation inserts a new motivation into the database
func postMotivation(c echo.Context) error {
	database := c.Get("db").(*db.DB)
	queue := c.Get("queue").(*MotivationQueue)

	// Read the request body
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.String(http.StatusBadRequest, "Error reading request body")
	}

	motivation := strings.TrimSpace(string(body))
	if motivation == "" {
		return c.String(http.StatusBadRequest, "Motivation cannot be empty")
	}

	// Insert into database
	id, err := database.Insert(motivation)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error saving motivation")
	}
	queue.Add(id, motivation)

	return c.String(http.StatusCreated, "Motivation added successfully")
}

// getMotivationPNG returns a random motivation as a PNG image
func getMotivationPNG(c echo.Context) error {
	queue := c.Get("queue").(*MotivationQueue)

	// Get the next motivation from the queue
	motivation, err := queue.Next()
	if err != nil {
		if strings.Contains(err.Error(), "no motivations found") {
			return c.String(http.StatusNotFound, "No motivations found")
		}
		return c.String(http.StatusInternalServerError, "Error retrieving motivation")
	}

	// Call the render service
	renderServiceURL := GetRenderServiceURL()
	renderURL := fmt.Sprintf("%s?text=%s", renderServiceURL, url.QueryEscape(motivation))

	resp, err := http.Get(renderURL)
	if err != nil {
		slog.Error("Failed to call render service", "error", err, "url", renderURL)
		return c.String(http.StatusInternalServerError, "Error rendering motivation image")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("Render service returned non-OK status", "status", resp.StatusCode, "url", renderURL)
		return c.String(http.StatusInternalServerError, "Error rendering motivation image")
	}

	// Read the image data from the render service
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("Failed to read image data from render service", "error", err)
		return c.String(http.StatusInternalServerError, "Error reading rendered image")
	}

	// Return the image data with appropriate content type
	return c.Blob(http.StatusOK, "image/png", imageData)
}

// listMotivations returns every motivation stored in the database as JSON.
func listMotivations(c echo.Context) error {
	database := c.Get("db").(*db.DB)

	motivations, err := database.GetAll()
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error retrieving motivations")
	}

	// GetAll returns nil (not an empty slice) when there are no rows, and
	// c.JSON marshals a nil slice as `null` rather than `[]`.
	if motivations == nil {
		motivations = make([]db.Motivation, 0)
	}

	return c.JSON(http.StatusOK, motivations)
}

// deleteMotivation removes a motivation by id from the database and, on
// success, evicts it from the in-memory queue so it stops being served.
func deleteMotivation(c echo.Context) error {
	database := c.Get("db").(*db.DB)
	queue := c.Get("queue").(*MotivationQueue)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid motivation id")
	}

	deleted, err := database.Delete(id)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error deleting motivation")
	}
	if !deleted {
		return c.String(http.StatusNotFound, "Motivation not found")
	}

	queue.Remove(id)

	return c.NoContent(http.StatusNoContent)
}

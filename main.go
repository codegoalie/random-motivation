package main

import (
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/codegoalie/random-motivation/db"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// MotivationQueue holds a shuffled list of motivations and the current position
type MotivationQueue struct {
	motivations []string
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

	return motivation, nil
}

// NewMotivationQueue creates a new queue from a list of motivations and shuffles them
func NewMotivationQueue(motivations []db.Motivation) *MotivationQueue {
	texts := make([]string, len(motivations))
	for i, m := range motivations {
		texts[i] = m.Text
	}

	// Shuffle the list
	rand.Shuffle(len(texts), func(i, j int) {
		texts[i], texts[j] = texts[j], texts[i]
	})

	return &MotivationQueue{
		motivations: texts,
		currentPos:  0,
	}
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
			"POST /motivation - Add a new motivation (send motivation text in request body)")
	})
	e.GET("/motivation", getMotivation)
	e.POST("/motivation", postMotivation)

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
	_, err = database.Insert(motivation)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error saving motivation")
	}

	return c.String(http.StatusCreated, "Motivation added successfully")
}

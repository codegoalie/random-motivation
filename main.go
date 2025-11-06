package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/codegoalie/random-motivation/db"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const motivationsFile = "motivations.txt"

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

	// Migrate data from text file if needed
	if err := database.MigrateFromTextFile(motivationsFile); err != nil {
		log.Fatalf("Failed to migrate from text file: %v", err)
	}

	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Store database in context
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("db", database)
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

// getMotivation returns a random motivation from the database
func getMotivation(c echo.Context) error {
	database := c.Get("db").(*db.DB)

	motivation, err := database.GetRandom()
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

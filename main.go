package main

import (
	"bufio"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const motivationsFile = "motivations.txt"

func main() {
	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Routes
	e.GET("/motivation", getMotivation)
	e.POST("/motivation", postMotivation)

	// Start server
	e.Logger.Fatal(e.Start(":8080"))
}

// getMotivation returns a random line from motivations.txt
func getMotivation(c echo.Context) error {
	// Read all lines from the file
	file, err := os.Open(motivationsFile)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error reading motivations file")
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return c.String(http.StatusInternalServerError, "Error reading motivations file")
	}

	if len(lines) == 0 {
		return c.String(http.StatusNotFound, "No motivations found")
	}

	// Select a random line
	rand.Seed(time.Now().UnixNano())
	randomLine := lines[rand.Intn(len(lines))]

	return c.String(http.StatusOK, randomLine)
}

// postMotivation appends the request body to motivations.txt
func postMotivation(c echo.Context) error {
	// Read the request body
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.String(http.StatusBadRequest, "Error reading request body")
	}

	motivation := strings.TrimSpace(string(body))
	if motivation == "" {
		return c.String(http.StatusBadRequest, "Motivation cannot be empty")
	}

	// Open the file in append mode
	file, err := os.OpenFile(motivationsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return c.String(http.StatusInternalServerError, "Error opening motivations file")
	}
	defer file.Close()

	// Append the new motivation with a newline
	if _, err := file.WriteString(motivation + "\n"); err != nil {
		return c.String(http.StatusInternalServerError, "Error writing to motivations file")
	}

	return c.String(http.StatusCreated, "Motivation added successfully")
}

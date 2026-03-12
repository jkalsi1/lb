package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func sleepHandler(w http.ResponseWriter, r *http.Request) {
	duration := r.URL.Query().Get("duration")
	if duration == "" {
		http.Error(w, "missing duration query param", http.StatusBadRequest)
		return
	}

	seconds, err := strconv.Atoi(duration)
	if err != nil {
		http.Error(w, "duration must be an integer", http.StatusBadRequest)
		return
	}

	time.Sleep(time.Duration(seconds) * time.Second)
	fmt.Fprintf(w, "slept for %d seconds\n", seconds)
}

func main() {
	port := "3031"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	http.HandleFunc("/sleep", sleepHandler)
	fmt.Printf("Server running on :%s\n", port)
	http.ListenAndServe(":"+port, nil)
}

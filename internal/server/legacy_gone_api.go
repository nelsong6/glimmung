package server

import "net/http"

func storageIDGone(message string) http.HandlerFunc {
	return legacyGone(message)
}

func legacyGone(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusGone, message)
	}
}

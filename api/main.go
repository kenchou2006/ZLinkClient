package handler

import (
	"net/http"

	"ZLinkClient/server"
)

var router http.Handler

func init() {
	cfg := server.LoadConfigFromEnv()
	db, rdb, _ := server.InitClientsOnce()
	router = server.GetOrBuildRouter(db, rdb, cfg)
}

func Handler(w http.ResponseWriter, req *http.Request) {
	// delegate to prebuilt router
	router.ServeHTTP(w, req)
}

func main() {
}

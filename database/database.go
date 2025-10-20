package database

import (
	"database/sql"
	"log"
	"os"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

func Connect() {
	// Pastikan folder data tersedia
	if _, err := os.Stat("./data"); os.IsNotExist(err) {
		err := os.Mkdir("./data", 0755)
		if err != nil {
			log.Fatal("Gagal buat folder data:", err)
		}
	}

	// Buat / buka database di folder data/
	dbPath := "./data/app.db"
	dsn := "file:" + dbPath + "?_pragma=busy_timeout(5000)"

	var err error
	DB, err = sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal("Gagal buka database:", err)
	}

	log.Println("Database siap di", dbPath)
}

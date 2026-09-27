package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: clearxdpos <chaindata_path>")
		os.Exit(1)
	}
	path := os.Args[1]
	db, err := leveldb.New(path, 16, 16, "xdpos", false)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	it := db.NewIterator([]byte("XDPoS-V2-"), nil)
	count := 0
	for it.Next() {
		key := string(it.Key())
		if strings.HasPrefix(key, "XDPoS-V2-") {
			if err := db.Delete(it.Key()); err != nil {
				log.Printf("Failed to delete key %s: %v", key, err)
			} else {
				count++
			}
		}
	}
	it.Release()
	fmt.Printf("Deleted %d XDPoS-V2 snapshot keys from %s\n", count, path)
}

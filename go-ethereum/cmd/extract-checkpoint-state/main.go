package main

import (
	"fmt"
	"os"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Println("Usage: extract-checkpoint-state <chaindata-dir> <state-root-hex> <output-file>")
		os.Exit(1)
	}
	chaindataDir := os.Args[1]
	stateRootHex := os.Args[2]
	outputFile := os.Args[3]
	stateRoot := common.HexToHash(stateRootHex)
	fmt.Printf("Extracting checkpoint state from %s, root %s\n", chaindataDir, stateRoot.Hex())

	// Open leveldb directly (read-only)
	db, err := leveldb.New(chaindataDir, 64, 64, "chaindata", true)
	if err != nil {
		fmt.Printf("Failed to open DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Collect all node hashes by BFS from state root
	nodeHashes := make(map[common.Hash]struct{})
	nodeHashes[stateRoot] = struct{}{}
	queue := []common.Hash{stateRoot}

	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]

		// Read node data using rawdb helper
		nodeData := rawdb.ReadLegacyTrieNode(db, hash)
		if len(nodeData) == 0 {
			continue
		}

		// Parse node to find child hashes
		children := parseNodeChildren(nodeData)
		for _, child := range children {
			if _, ok := nodeHashes[child]; !ok {
				nodeHashes[child] = struct{}{}
				queue = append(queue, child)
			}
		}
	}

	fmt.Printf("Collected %d node hashes\n", len(nodeHashes))

	output, err := os.Create(outputFile)
	if err != nil {
		fmt.Printf("Failed to create output: %v\n", err)
		os.Exit(1)
	}
	defer output.Close()

	// Write header: state root (32 bytes) + node count placeholder (4 bytes) + padding
	header := make([]byte, 64)
	copy(header[0:32], stateRoot.Bytes())
	output.Write(header)

	count := 0
	for hash := range nodeHashes {
		nodeData := rawdb.ReadLegacyTrieNode(db, hash)
		if len(nodeData) == 0 {
			fmt.Printf("Node not found: %s\n", hash.Hex())
			continue
		}
		output.Write(hash.Bytes())
		lenBytes := make([]byte, 4)
		lenBytes[0] = byte(len(nodeData) >> 24)
		lenBytes[1] = byte(len(nodeData) >> 16)
		lenBytes[2] = byte(len(nodeData) >> 8)
		lenBytes[3] = byte(len(nodeData))
		output.Write(lenBytes)
		output.Write(nodeData)
		count++
	}

	// Update node count in header
	output.Seek(32, 0)
	countBytes := make([]byte, 4)
	countBytes[0] = byte(count >> 24)
	countBytes[1] = byte(count >> 16)
	countBytes[2] = byte(count >> 8)
	countBytes[3] = byte(count)
	output.Write(countBytes)

	fmt.Printf("Extracted %d nodes to %s\n", count, outputFile)
}

// parseNodeChildren extracts child node hashes from an RLP-encoded trie node
func parseNodeChildren(data []byte) []common.Hash {
	var children []common.Hash
	if len(data) < 32 {
		return children
	}

	// Decode RLP list
	if data[0] >= 0xc0 {
		// It's a list (branch or extension node)
		listLen, offset := decodeLength(data)
		if listLen == 0 {
			return children
		}
		
		// For branch nodes: 17 items (16 children + value)
		// For extension nodes: 2 items (prefix + child)
		pos := offset
		itemCount := 0
		for pos < len(data) && itemCount < 17 {
			item, newPos, err := decodeRLPItem(data[pos:])
			if err != nil {
				break
			}
			
			// If item is a hash reference (32 bytes), it's a child node
			if len(item) == 32 {
				children = append(children, common.BytesToHash(item))
			} else if len(item) == 1 && item[0] < 0x80 {
				// Empty item (0x80), skip
			}
			
			pos += newPos
			itemCount++
		}
	}
	
	return children
}

func decodeLength(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	prefix := data[0]
	if prefix < 0x80 {
		return 1, 1
	} else if prefix < 0xb8 {
		return int(prefix - 0x80), 1
	} else if prefix < 0xc0 {
		lenLen := int(prefix - 0xb7)
		if len(data) < 1+lenLen {
			return 0, 0
		}
		length := 0
		for i := 0; i < lenLen; i++ {
			length = length*256 + int(data[1+i])
		}
		return length, 1 + lenLen
	} else if prefix < 0xf8 {
		return int(prefix - 0xc0), 1
	} else {
		lenLen := int(prefix - 0xf7)
		if len(data) < 1+lenLen {
			return 0, 0
		}
		length := 0
		for i := 0; i < lenLen; i++ {
			length = length*256 + int(data[1+i])
		}
		return length, 1 + lenLen
	}
}

func decodeRLPItem(data []byte) ([]byte, int, error) {
	if len(data) == 0 {
		return nil, 0, fmt.Errorf("empty data")
	}
	prefix := data[0]
	
	if prefix < 0x80 {
		// Single byte
		return data[:1], 1, nil
	} else if prefix < 0xb8 {
		// Short string
		length := int(prefix - 0x80)
		if len(data) < 1+length {
			return nil, 0, fmt.Errorf("short data")
		}
		return data[1:1+length], 1 + length, nil
	} else if prefix < 0xc0 {
		// Long string
		lenLen := int(prefix - 0xb7)
		if len(data) < 1+lenLen {
			return nil, 0, fmt.Errorf("short data")
		}
		length := 0
		for i := 0; i < lenLen; i++ {
			length = length*256 + int(data[1+i])
		}
		if len(data) < 1+lenLen+length {
			return nil, 0, fmt.Errorf("short data")
		}
		return data[1+lenLen : 1+lenLen+length], 1 + lenLen + length, nil
	} else if prefix < 0xf8 {
		// Short list
		length := int(prefix - 0xc0)
		if len(data) < 1+length {
			return nil, 0, fmt.Errorf("short data")
		}
		return data[1:1+length], 1 + length, nil
	} else {
		// Long list
		lenLen := int(prefix - 0xf7)
		if len(data) < 1+lenLen {
			return nil, 0, fmt.Errorf("short data")
		}
		length := 0
		for i := 0; i < lenLen; i++ {
			length = length*256 + int(data[1+i])
		}
		if len(data) < 1+lenLen+length {
			return nil, 0, fmt.Errorf("short data")
		}
		return data[1+lenLen : 1+lenLen+length], 1 + lenLen + length, nil
	}
}

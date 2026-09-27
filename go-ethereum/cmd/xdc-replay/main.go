package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// Config holds CLI flags
type Config struct {
	Network   string
	From      uint64
	To        uint64
	Block     uint64
	Archive   string
	CI        bool
	Fixture   string
	Format    string
}

func main() {
	cfg := parseFlags()

	if cfg.CI {
		runCI(cfg)
		return
	}

	if cfg.Block > 0 {
		runSingleBlock(cfg)
		return
	}

	runRange(cfg)
}

func parseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.Network, "network", "apothem", "Network: mainnet or apothem")
	flag.Uint64Var(&cfg.From, "from", 0, "Start block number")
	flag.Uint64Var(&cfg.To, "to", 0, "End block number")
	flag.Uint64Var(&cfg.Block, "block", 0, "Single block to compare")
	flag.StringVar(&cfg.Archive, "archive", "", "Archive node RPC URL")
	flag.BoolVar(&cfg.CI, "ci", false, "CI mode (uses embedded fixture)")
	flag.StringVar(&cfg.Fixture, "fixture", "", "Fixture name for CI mode")
	flag.StringVar(&cfg.Format, "format", "md", "Output format: json or md")
	flag.Parse()
	return cfg
}

// ArchiveClient connects to v2.6.8 archive node for ground truth
type ArchiveClient struct {
	client *rpc.Client
}

func NewArchiveClient(url string) (*ArchiveClient, error) {
	client, err := rpc.Dial(url)
	if err != nil {
		return nil, err
	}
	return &ArchiveClient{client: client}, nil
}

func (a *ArchiveClient) GetBlockByNumber(num uint64) (*types.Block, error) {
	var result *types.Block
	err := a.client.Call(&result, "eth_getBlockByNumber", fmt.Sprintf("0x%x", num), true)
	return result, err
}

func (a *ArchiveClient) GetReceiptsByHash(hash common.Hash) (types.Receipts, error) {
	var result types.Receipts
	err := a.client.Call(&result, "eth_getBlockReceipts", hash)
	return result, err
}

// DiffReport holds comparison results
type DiffReport struct {
	BlockNumber   uint64            `json:"block_number"`
	BlockHash     string            `json:"block_hash"`
	ArchiveHash   string            `json:"archive_hash"`
	StateRoot     string            `json:"state_root"`
	ArchiveState  string            `json:"archive_state_root"`
	ReceiptsRoot  string            `json:"receipts_root"`
	ArchiveRcpt   string            `json:"archive_receipts_root"`
	Author        string            `json:"author"`
	ArchiveAuthor string            `json:"archive_author"`
	Diffs         []string          `json:"diffs"`
	Timestamp     time.Time         `json:"timestamp"`
}

func (r *DiffReport) HasDiff() bool {
	return len(r.Diffs) > 0
}

func runSingleBlock(cfg Config) {
	log.Printf("Comparing single block %d on %s", cfg.Block, cfg.Network)
	
	if cfg.Archive == "" {
		log.Fatal("--archive required for single-block mode")
	}

	archive, err := NewArchiveClient(cfg.Archive)
	if err != nil {
		log.Fatalf("Failed to connect to archive: %v", err)
	}

	block, err := archive.GetBlockByNumber(cfg.Block)
	if err != nil {
		log.Fatalf("Failed to fetch archive block: %v", err)
	}

	report := DiffReport{
		BlockNumber:   cfg.Block,
		ArchiveHash:   block.Hash().Hex(),
		ArchiveState:  block.Root().Hex(),
		ArchiveRcpt:   block.ReceiptHash().Hex(),
		Timestamp:     time.Now(),
	}

	// TODO: Import block into ephemeral GP5 chain and compare
	// This requires full chain setup - stubbed for Phase 1 scaffolding
	report.Diffs = append(report.Diffs, "GP5 ephemeral chain not yet implemented")

	outputReport(report, cfg.Format)
}

func runRange(cfg Config) {
	log.Printf("Comparing blocks %d to %d on %s", cfg.From, cfg.To, cfg.Network)
	
	if cfg.Archive == "" {
		log.Fatal("--archive required for range mode")
	}

	archive, err := NewArchiveClient(cfg.Archive)
	if err != nil {
		log.Fatalf("Failed to connect to archive: %v", err)
	}

	var reports []DiffReport
	var diffCount int

	for num := cfg.From; num <= cfg.To; num++ {
		block, err := archive.GetBlockByNumber(num)
		if err != nil {
			log.Printf("Block %d: archive fetch error: %v", num, err)
			continue
		}

		report := DiffReport{
			BlockNumber:   num,
			ArchiveHash:   block.Hash().Hex(),
			ArchiveState:  block.Root().Hex(),
			ArchiveRcpt:   block.ReceiptHash().Hex(),
			Timestamp:     time.Now(),
		}

		// TODO: Import into GP5 ephemeral chain
		report.Diffs = append(report.Diffs, "GP5 ephemeral chain not yet implemented")

		if report.HasDiff() {
			diffCount++
		}
		reports = append(reports, report)
	}

	log.Printf("Range complete: %d blocks checked, %d with diffs", len(reports), diffCount)
	
	if cfg.Format == "json" {
		data, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("\n## Range Report: %d → %d\n\n", cfg.From, cfg.To)
		fmt.Printf("| Block | Archive Hash | Diffs |\n")
		fmt.Printf("|-------|-------------|-------|\n")
		for _, r := range reports {
			diffStr := "✅"
			if r.HasDiff() {
				diffStr = "❌ " + fmt.Sprintf("%d", len(r.Diffs))
			}
			fmt.Printf("| %d | %s | %s |\n", r.BlockNumber, r.ArchiveHash[:18]+"...", diffStr)
		}
	}
}

func runCI(cfg Config) {
	log.Println("CI mode: running against embedded fixture")
	
	// TODO: Load embedded fixture and run comparison
	// Fixture data would be go:embed'd JSON with pre-fetched archive blocks
	
	fmt.Println("CI mode not yet implemented - fixture system pending")
	os.Exit(1)
}

func outputReport(report DiffReport, format string) {
	if format == "json" {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("\n## Block %d Report\n\n", report.BlockNumber)
		fmt.Printf("- Archive Hash: %s\n", report.ArchiveHash)
		fmt.Printf("- Diffs: %d\n", len(report.Diffs))
		for _, d := range report.Diffs {
			fmt.Printf("  - %s\n", d)
		}
	}
}

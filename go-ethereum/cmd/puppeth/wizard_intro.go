// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/log"
)

// makeWizard creates and returns a new puppeth wizard.
func makeWizard(network string) *wizard {
	return &wizard{
		network: network,
		conf:    config{},
		in:      bufio.NewReader(os.Stdin),
	}
}

// run is the main puppeth loop, reduced to genesis creation only.
//
// The legacy XDPoSChain puppeth also exposed:
//   - Show network stats (SSH probe of deployed components)
//   - Track / manage remote servers (SSH dial)
//   - Deploy / manage network components (Docker Compose generators for
//     ethstats / explorer / wallet / faucet / nginx / node)
//
// These deployment wizards are intentionally not ported; they have bit-rotted
// vs the upstream geth 1.17 base and are orthogonal to the core need
// (producing a JSON genesis with the XDC system contracts deployed).
func (w *wizard) run() {
	fmt.Println("+-------------------------------------------------------------+")
	fmt.Println("| Welcome to puppeth — XDPoS private network genesis assembler |")
	fmt.Println("|                                                             |")
	fmt.Println("| This tool produces a JSON genesis with the four XDC system  |")
	fmt.Println("| contracts (XDCValidator, BlockSigner, Randomize, MultiSig)  |")
	fmt.Println("| pre-deployed for the operator-supplied signer set.          |")
	fmt.Println("+-------------------------------------------------------------+")
	fmt.Println()

	if w.network == "" {
		fmt.Println("Please specify a network name to administer (no spaces or hyphens, please)")
		for {
			w.network = w.readString()
			if !strings.Contains(w.network, " ") && !strings.Contains(w.network, "-") {
				fmt.Printf("\nSweet, you can set this via --network=%s next time!\n\n", w.network)
				break
			}
			log.Error("No spaces or hyphens allowed in network name, retry")
		}
	}
	log.Info("Administering XDPoS network", "name", w.network)

	w.conf.path = filepath.Join(os.Getenv("HOME"), ".puppeth", w.network)
	if blob, err := os.ReadFile(w.conf.path); err == nil {
		if err := json.Unmarshal(blob, &w.conf); err != nil {
			log.Crit("Previous configuration corrupted", "path", w.conf.path, "err", err)
		}
		log.Info("Loaded prior genesis configuration", "path", w.conf.path)
	} else {
		log.Warn("No previous configurations found", "path", w.conf.path)
	}

	for {
		fmt.Println()
		if w.conf.Genesis == nil {
			fmt.Println("What would you like to do? (default = configure)")
			fmt.Println(" 1. Configure new genesis")
			fmt.Println(" 2. Exit")
		} else {
			fmt.Println("What would you like to do? (default = manage)")
			fmt.Println(" 1. Manage existing genesis")
			fmt.Println(" 2. Exit")
		}
		choice := w.read()
		switch choice {
		case "1", "":
			if w.conf.Genesis == nil {
				w.makeGenesis()
			} else {
				w.manageGenesis()
			}
		case "2":
			return
		default:
			log.Error("That's not something I can do")
		}
	}
}

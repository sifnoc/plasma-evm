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
	"encoding/json"
	"fmt"
	"time"

	"github.com/Onther-Tech/plasma-evm/accounts/keystore"
	"github.com/Onther-Tech/plasma-evm/log"
)

// deployNode creates a new node configuration based on some user input.
func (w *wizard) deployNode(user bool) {
	// Do some sanity check before the user wastes time on input
	if w.conf.Genesis == nil {
		log.Error("No genesis block configured")
		return
	}
	if w.conf.ethstats == "" {
		log.Error("No ethstats server configured")
		return
	}
	// Select the server to interact with
	server := w.selectServer()
	if server == "" {
		return
	}
	client := w.servers[server]

	// Retrieve any active node configurations from the server
	infos, err := checkNode(client, w.network, user)
	if err != nil {
		if user {
			infos = &nodeInfos{port: 30305, peersTotal: 512, peersLight: 256}
		} else {
			infos = &nodeInfos{port: 30305, peersTotal: 50, peersLight: 0, gasTarget: 7.5, gasLimit: 10, gasPrice: 1}
		}
	}
	existed := err == nil

	infos.genesis, _ = json.MarshalIndent(w.conf.Genesis, "", "  ")
	infos.network = w.conf.Genesis.Config.ChainID.Int64()

	// Figure out which URL to listen for root chain JSONRPC endpoint
	fmt.Println()
	fmt.Printf("What URL to listen on root chain JSONRPC?\n")
	infos.rootchainURL = w.readURL().String()

	// Figure out whether expose JSONRPC or not

	fmt.Println()
	fmt.Printf("Do you want expose HTTP JSONRPC endpoint (y/n)? (default=no)\n")
	if w.readDefaultYesNo(false) {
		fmt.Printf("Which TCP port to expose? (default=8545)\n")
		infos.rpcPort = w.readDefaultInt(8545)

		vhost := fmt.Sprintf("%s,localhost", client.server)
		fmt.Printf("Which is virtual hostname? (default=%s)\n", vhost)
		infos.vhost = w.readDefaultString(vhost)
	}
	fmt.Println()
	fmt.Printf("Do you want expose WebSocket JSONRPC endpoint (y/n)? (default=no)\n")
	if w.readDefaultYesNo(false) {
		fmt.Printf("Which TCP port to expose? (default=8546)\n")
		infos.wsPort = w.readDefaultInt(8546)
	}

	// Figure out where the user wants to store the persistent data
	fmt.Println()
	if infos.datadir == "" {
		fmt.Printf("Where should data be stored on the remote machine?\n")
		infos.datadir = w.readString()
	} else {
		fmt.Printf("Where should data be stored on the remote machine? (default = %s)\n", infos.datadir)
		infos.datadir = w.readDefaultString(infos.datadir)
	}
	if w.conf.Genesis.Config.Ethash != nil && !user {
		fmt.Println()
		if infos.ethashdir == "" {
			fmt.Printf("Where should the ethash mining DAGs be stored on the remote machine?\n")
			infos.ethashdir = w.readString()
		} else {
			fmt.Printf("Where should the ethash mining DAGs be stored on the remote machine? (default = %s)\n", infos.ethashdir)
			infos.ethashdir = w.readDefaultString(infos.ethashdir)
		}
	}
	// Figure out which port to listen on
	fmt.Println()
	fmt.Printf("Which TCP/UDP port to listen on? (default = %d)\n", infos.port)
	infos.port = w.readDefaultInt(infos.port)

	// Figure out how many peers to allow (different based on node type)
	fmt.Println()
	fmt.Printf("How many peers to allow connecting? (default = %d)\n", infos.peersTotal)
	infos.peersTotal = w.readDefaultInt(infos.peersTotal)

	// Figure out how many light peers to allow (different based on node type)
	fmt.Println()
	fmt.Printf("How many light peers to allow connecting? (default = %d)\n", infos.peersLight)
	infos.peersLight = w.readDefaultInt(infos.peersLight)

	// Set a proper name to report on the stats page
	fmt.Println()
	if infos.ethstats == "" {
		fmt.Printf("What should the node be called on the stats page?\n")
		infos.ethstats = w.readString() + ":" + w.conf.ethstats
	} else {
		fmt.Printf("What should the node be called on the stats page? (default = %s)\n", infos.ethstats)
		infos.ethstats = w.readDefaultString(infos.ethstats) + ":" + w.conf.ethstats
	}
	// If the node is an operator, load up needed credentials
	if !user {
		if w.conf.Genesis.Config.Ethash != nil {
			// If a previous operator was already set, offer to reuse it
			if infos.operatorKeyJSON != "" {
				if key, err := keystore.DecryptKey([]byte(infos.operatorKeyJSON), infos.operatorKeyPass); err != nil {
					infos.operatorKeyJSON, infos.operatorKeyPass = "", ""
				} else {
					fmt.Println()
					fmt.Printf("Reuse previous (%s) signing account (y/n)? (default = yes)\n", key.Address.Hex())
					if !w.readDefaultYesNo(true) {
						infos.operatorKeyJSON, infos.operatorKeyPass = "", ""
					}
				}
			}
			// Ethash based miners only need an etherbase to mine against
			if infos.operatorKeyJSON == "" {
				fmt.Println()
				fmt.Println("Please paste the operator's key JSON:")
				infos.operatorKeyJSON = w.readJSON()

				fmt.Println()
				fmt.Println("What's the unlock password for the account? (won't be echoed)")
				infos.operatorKeyPass = w.readPassword()

				if _, err := keystore.DecryptKey([]byte(infos.operatorKeyJSON), infos.operatorKeyPass); err != nil {
					log.Error("Failed to decrypt key with given passphrase")
					return
				}
			}

			// If a previous challenger was already set, offer to reuse it
			if infos.challengerKeyJSON != "" {
				if key, err := keystore.DecryptKey([]byte(infos.challengerKeyJSON), infos.challengerKeyPass); err != nil {
					infos.challengerKeyJSON, infos.challengerKeyPass = "", ""
				} else {
					fmt.Println()
					fmt.Printf("Reuse previous (%s) signing account (y/n)? (default = yes)\n", key.Address.Hex())
					if !w.readDefaultYesNo(true) {
						infos.challengerKeyJSON, infos.challengerKeyPass = "", ""
					}
				}
			}
			// Ethash based miners only need an etherbase to mine against
			if infos.challengerKeyJSON == "" {
				fmt.Println()
				fmt.Println("Please paste the challenger's key JSON:")
				infos.challengerKeyJSON = w.readJSON()

				fmt.Println()
				fmt.Println("What's the unlock password for the account? (won't be echoed)")
				infos.challengerKeyPass = w.readPassword()

				if _, err := keystore.DecryptKey([]byte(infos.challengerKeyJSON), infos.challengerKeyPass); err != nil {
					log.Error("Failed to decrypt key with given passphrase")
					return
				}
			}

			if infos.challengerKeyJSON != "" && infos.challengerKeyJSON == infos.operatorKeyJSON {
				log.Error("Cannot use same address as challenger and operator.")
				return
			}
		} else if w.conf.Genesis.Config.Clique != nil { // TODO: disable clique
			// If a previous signer was already set, offer to reuse it
			if infos.operatorKeyJSON != "" {
				if key, err := keystore.DecryptKey([]byte(infos.operatorKeyJSON), infos.operatorKeyPass); err != nil {
					infos.operatorKeyJSON, infos.operatorKeyPass = "", ""
				} else {
					fmt.Println()
					fmt.Printf("Reuse previous (%s) signing account (y/n)? (default = yes)\n", key.Address.Hex())
					if !w.readDefaultYesNo(true) {
						infos.operatorKeyJSON, infos.operatorKeyPass = "", ""
					}
				}
			}
			// Clique based signers need a keyfile and unlock password, ask if unavailable
			if infos.operatorKeyJSON == "" {
				fmt.Println()
				fmt.Println("Please paste the signer's key JSON:")
				infos.operatorKeyJSON = w.readJSON()

				fmt.Println()
				fmt.Println("What's the unlock password for the account? (won't be echoed)")
				infos.operatorKeyPass = w.readPassword()

				if _, err := keystore.DecryptKey([]byte(infos.operatorKeyJSON), infos.operatorKeyPass); err != nil {
					log.Error("Failed to decrypt key with given password")
					return
				}
			}
		}
		// Establish the gas dynamics to be enforced by the signer
		fmt.Println()
		fmt.Printf("What gas limit should empty blocks target (MGas)? (default = %0.3f)\n", infos.gasTarget)
		infos.gasTarget = w.readDefaultFloat(infos.gasTarget)

		fmt.Println()
		fmt.Printf("What gas limit should full blocks target (MGas)? (default = %0.3f)\n", infos.gasLimit)
		infos.gasLimit = w.readDefaultFloat(infos.gasLimit)

		fmt.Println()
		fmt.Printf("What gas price should the operator require (GWei)? (default = %0.3f)\n", infos.gasPrice)
		infos.gasPrice = w.readDefaultFloat(infos.gasPrice)
	}
	// Try to deploy the full node on the host
	nocache := false
	if existed {
		fmt.Println()
		fmt.Printf("Should the node be built from scratch (y/n)? (default = no)\n")
		nocache = w.readDefaultYesNo(false)
	}
	if out, err := deployNode(client, w.network, w.images["node"], w.conf.bootnodes, infos, nocache); err != nil {
		log.Error("Failed to deploy Ethereum node container", "err", err)
		if len(out) > 0 {
			fmt.Printf("%s\n", out)
		}
		return
	}
	// All ok, run a network scan to pick any changes up
	log.Info("Waiting for node to finish booting")
	time.Sleep(3 * time.Second)

	w.networkStats()
}

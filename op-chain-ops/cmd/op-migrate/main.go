package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-bindings/hardhat"

	"github.com/ethereum-optimism/optimism/op-chain-ops/genesis"
	"github.com/ethereum-optimism/optimism/op-chain-ops/genesis/migration"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/mattn/go-isatty"
	"github.com/urfave/cli"
)

func main() {
	log.Root().SetHandler(log.StreamHandler(os.Stderr, log.TerminalFormat(isatty.IsTerminal(os.Stderr.Fd()))))

	app := &cli.App{
		Name:  "migrate",
		Usage: "Migrate a legacy database",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "l1-rpc-url",
				Value: "http://127.0.0.1:8545",
				Usage: "RPC URL for an L1 Node",
			},
			&cli.StringFlag{
				Name:  "ovm-addresses",
				Usage: "Path to ovm-addresses.json",
			},
			&cli.StringFlag{
				Name:  "evm-addresses",
				Usage: "Path to evm-addresses.json",
			},
			&cli.StringFlag{
				Name:  "ovm-allowances",
				Usage: "Path to ovm-allowances.json",
			},
			&cli.StringFlag{
				Name:  "ovm-messages",
				Usage: "Path to ovm-messages.json",
			},
			&cli.StringFlag{
				Name:  "evm-messages",
				Usage: "Path to evm-messages.json",
			},
			&cli.StringFlag{
				Name:  "db-path",
				Usage: "Path to database",
			},
			cli.StringFlag{
				Name:  "deploy-config",
				Usage: "Path to hardhat deploy config file",
			},
			cli.StringFlag{
				Name:  "network",
				Usage: "Name of hardhat deploy network",
			},
			cli.StringFlag{
				Name:  "hardhat-deployments",
				Usage: "Comma separated list of hardhat deployment directories",
			},
			cli.BoolFlag{
				Name:  "dry-run",
				Usage: "Dry run the upgrade by not committing the database",
			},
			cli.BoolFlag{
				Name:  "no-check",
				Usage: "Do not perform sanity checks. This should only be used for testing",
			},
			cli.IntFlag{
				Name:  "db-cache",
				Usage: "LevelDB cache size in mb",
				Value: 1024,
			},
			cli.IntFlag{
				Name:  "db-handles",
				Usage: "LevelDB number of handles",
				Value: 60,
			},
			cli.StringFlag{
				Name:  "rollup-config-out",
				Usage: "Path that op-node config will be written to disk",
				Value: "rollup.json",
			},
		},
		Action: func(ctx *cli.Context) error {
			deployConfig := ctx.String("deploy-config")
			config, err := genesis.NewDeployConfig(deployConfig)
			if err != nil {
				return err
			}

			ovmAddresses, err := migration.NewAddresses(ctx.String("ovm-addresses"))
			if err != nil {
				return err
			}
			evmAddresess, err := migration.NewAddresses(ctx.String("evm-addresses"))
			if err != nil {
				return err
			}
			ovmAllowances, err := migration.NewAllowances(ctx.String("ovm-allowances"))
			if err != nil {
				return err
			}
			ovmMessages, err := migration.NewSentMessage(ctx.String("ovm-messages"))
			if err != nil {
				return err
			}
			evmMessages, err := migration.NewSentMessage(ctx.String("evm-messages"))
			if err != nil {
				return err
			}

			migrationData := migration.MigrationData{
				OvmAddresses:  ovmAddresses,
				EvmAddresses:  evmAddresess,
				OvmAllowances: ovmAllowances,
				OvmMessages:   ovmMessages,
				EvmMessages:   evmMessages,
			}

			network := ctx.String("network")
			deployments := strings.Split(ctx.String("hardhat-deployments"), ",")
			hh, err := hardhat.New(network, []string{}, deployments)
			if err != nil {
				return err
			}

			l1RpcURL := ctx.String("l1-rpc-url")
			l1Client, err := ethclient.Dial(l1RpcURL)
			if err != nil {
				return err
			}

			var block *types.Block
			tag := config.L1StartingBlockTag
			if tag.BlockNumber != nil {
				block, err = l1Client.BlockByNumber(context.Background(), big.NewInt(tag.BlockNumber.Int64()))
			} else if tag.BlockHash != nil {
				block, err = l1Client.BlockByHash(context.Background(), *tag.BlockHash)
			} else {
				return fmt.Errorf("invalid l1StartingBlockTag in deploy config: %v", tag)
			}
			if err != nil {
				return err
			}

			dbCache := ctx.Int("db-cache")
			dbHandles := ctx.Int("db-handles")

			chaindataPath := filepath.Join(ctx.String("db-path"), "geth", "chaindata")
			ancientPath := filepath.Join(chaindataPath, "ancient")
			ldb, err := rawdb.NewLevelDBDatabaseWithFreezer(chaindataPath, dbCache, dbHandles, ancientPath, "", false)
			if err != nil {
				return err
			}

			// Read the required deployment addresses from disk if required
			if err := config.GetDeployedAddresses(hh); err != nil {
				return err
			}

			if err := config.Check(); err != nil {
				return err
			}

			dryRun := ctx.Bool("dry-run")
			noCheck := ctx.Bool("no-check")
			// Perform the migration
			res, err := genesis.MigrateDB(ldb, config, block, &migrationData, !dryRun, noCheck)
			if err != nil {
				return err
			}

			// Close the database handle
			if err := ldb.Close(); err != nil {
				return err
			}

			postLDB, err := rawdb.NewLevelDBDatabaseWithFreezer(chaindataPath, dbCache, dbHandles, ancientPath, "", false)
			if err != nil {
				return err
			}

			if err := genesis.PostCheckMigratedDB(postLDB, migrationData, &config.L1CrossDomainMessengerProxy, config.L1ChainID); err != nil {
				return err
			}

			if err := postLDB.Close(); err != nil {
				return err
			}

			opNodeConfig, err := config.RollupConfig(block, res.TransitionBlockHash, res.TransitionHeight)
			if err != nil {
				return err
			}

			if err := writeJSON(ctx.String("rollup-config-out"), opNodeConfig); err != nil {
				return err
			}

			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Crit("error in migration", "err", err)
	}
}

func writeJSON(outfile string, input interface{}) error {
	f, err := os.OpenFile(outfile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(input)
}

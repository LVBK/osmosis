package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/suite"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"

	"github.com/osmosis-labs/osmosis/v7/tests/e2e/chain"
	dockerconfig "github.com/osmosis-labs/osmosis/v7/tests/e2e/docker"
	"github.com/osmosis-labs/osmosis/v7/tests/e2e/util"
)

type status struct {
	LatestHeight string `json:"latest_block_height"`
}

type syncInfo struct {
	SyncInfo status `json:"SyncInfo"`
}

type chainConfig struct {
	// voting period is number of blocks it takes to deposit, 1.2 seconds per validator to vote on the prop, and a buffer.
	votingPeriod float32
	// upgrade proposal height for chain.
	propHeight int
	// Indexes of the validators to skip from running during initialization.
	// This is needed for testing functionality like state-sync where we would
	// like to start a node during tests post-initialization.
	skipRunValidatorIndexes map[int]struct{}
	chain                   *chain.Chain
}

const (
	// osmosis version being upgraded to (folder must exist here https://github.com/osmosis-labs/osmosis/tree/main/app/upgrades)
	upgradeVersion = "v9"
	// estimated number of blocks it takes to submit for a proposal
	propSubmitBlocks float32 = 10
	// estimated number of blocks it takes to deposit for a proposal
	propDepositBlocks float32 = 10
	// number of blocks it takes to vote for a single validator to vote for a proposal
	propVoteBlocks float32 = 1.2
	// number of blocks used as a calculation buffer
	propBufferBlocks float32 = 5
	// max retries for json unmarshalling
	maxRetries = 60
	// Environment variable name to skip the upgrade tests
	skipUpgradeEnv = "OSMOSIS_E2E_SKIP_UPGRADE"
	// Environment variable name to skip the IBC tests
	skipIBCEnv = "OSMOSIS_E2E_SKIP_IBC"
	// Environment variable name to skip cleaning up Docker resources in teardown.
	skipCleanupEnv = "OSMOSIS_E2E_SKIP_CLEANUP"
)

var (
	// whatever number of validator configs get posted here are how many validators that will spawn on chain A and B respectively
	validatorConfigsChainA = []*chain.ValidatorConfig{
		{
			Pruning:            "default",
			PruningKeepRecent:  "0",
			PruningInterval:    "0",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
		{
			Pruning:            "nothing",
			PruningKeepRecent:  "0",
			PruningInterval:    "0",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
		{
			Pruning:            "custom",
			PruningKeepRecent:  "10000",
			PruningInterval:    "13",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
		{
			Pruning:            "everything",
			PruningKeepRecent:  "0",
			PruningInterval:    "0",
			SnapshotInterval:   0,
			SnapshotKeepRecent: 0,
		},
	}
	validatorConfigsChainB = []*chain.ValidatorConfig{
		{
			Pruning:            "default",
			PruningKeepRecent:  "0",
			PruningInterval:    "0",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
		{
			Pruning:            "nothing",
			PruningKeepRecent:  "0",
			PruningInterval:    "0",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
		{
			Pruning:            "custom",
			PruningKeepRecent:  "10000",
			PruningInterval:    "13",
			SnapshotInterval:   1500,
			SnapshotKeepRecent: 2,
		},
	}
)

type IntegrationTestSuite struct {
	suite.Suite

	tmpDirs        []string
	chainConfigs   []*chainConfig
	dkrPool        *dockertest.Pool
	dkrNet         *dockertest.Network
	hermesResource *dockertest.Resource
	initResource   *dockertest.Resource
	valResources   map[string][]*dockertest.Resource
	dockerImages   dockerconfig.ImageConfig
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}

func (s *IntegrationTestSuite) SetupSuite() {
	s.T().Log("setting up e2e integration test suite...")

	s.chainConfigs = make([]*chainConfig, 0, 2)

	// The e2e test flow is as follows:
	//
	// 1. Configure two chains - chan A and chain B.
	//   * For each chain, set up two validators
	//   * Initialize configs and genesis for all validators.
	// 2. Start both networks.
	// 3. Run IBC relayer betweeen the two chains.
	// 4. Execute various e2e tests, including IBC.
	var (
		skipUpgrade bool
		skipIBC     bool
		err         error
	)

	if str := os.Getenv(skipUpgradeEnv); len(str) > 0 {
		skipUpgrade, err = strconv.ParseBool(str)
		s.Require().NoError(err)
		s.T().Log(fmt.Sprintf("%s was true, skipping upgrafe tests", skipIBCEnv))
	}

	if str := os.Getenv(skipIBCEnv); len(str) > 0 {
		skipIBC, err = strconv.ParseBool(str)
		s.Require().NoError(err)
		s.T().Log(fmt.Sprintf("%s was true, skipping IBC tests", skipIBCEnv))
	}

	if skipIBC && !skipUpgrade {
		s.T().Fatalf(fmt.Sprintf("IBC tests must be enabled for upgrade tests, either set %s to false or %s to true", skipIBCEnv, skipUpgradeEnv))
	}

	s.dockerImages = *dockerconfig.NewImageConfig(!skipUpgrade)

	s.configureDockerResources(chain.ChainAID, chain.ChainBID)

	s.configureChain(chain.ChainAID, validatorConfigsChainA, map[int]struct{}{
		3: {}, // skip validator at index 3
	})

	if !skipIBC {
		// Configure second chain for IBC tests only.
		s.configureChain(chain.ChainBID, validatorConfigsChainB, map[int]struct{}{})
	}

	for i, chainConfig := range s.chainConfigs {
		s.runValidators(chainConfig, s.dockerImages.OsmosisRepository, s.dockerImages.OsmosisTag, i*10)
	}

	if !skipIBC {
		// Run a relayer between every possible pair of chains.
		for i := 0; i < len(s.chainConfigs); i++ {
			for j := i + 1; j < len(s.chainConfigs); j++ {
				s.runIBCRelayer(s.chainConfigs[i].chain, s.chainConfigs[j].chain)
			}
		}
	}

	if !skipUpgrade {
		s.createPreUpgradeState()
		s.upgrade()
		s.runPostUpgradeTests()
	}
}

func (s *IntegrationTestSuite) TearDownSuite() {
	if str := os.Getenv(skipCleanupEnv); len(str) > 0 {
		skipCleanup, err := strconv.ParseBool(str)
		s.Require().NoError(err)

		if skipCleanup {
			return
		}
	}

	s.T().Log("tearing down e2e integration test suite...")

	s.Require().NoError(s.dkrPool.Purge(s.hermesResource))

	for _, vr := range s.valResources {
		for _, r := range vr {
			s.Require().NoError(s.dkrPool.Purge(r))
		}
	}

	s.Require().NoError(s.dkrPool.RemoveNetwork(s.dkrNet))

	for _, chainConfig := range s.chainConfigs {
		os.RemoveAll(chainConfig.chain.ChainMeta.DataDir)
	}

	for _, td := range s.tmpDirs {
		os.RemoveAll(td)
	}
}

func (s *IntegrationTestSuite) runValidators(chainConfig *chainConfig, dockerRepository, dockerTag string, portOffset int) {
	chain := chainConfig.chain
	s.T().Logf("starting %s validator containers...", chain.ChainMeta.Id)
	s.valResources[chain.ChainMeta.Id] = make([]*dockertest.Resource, len(chain.Validators)-len(chainConfig.skipRunValidatorIndexes))
	pwd, err := os.Getwd()
	s.Require().NoError(err)
	for i, val := range chain.Validators {
		// Skip some validators from running during set up.
		// This is needed for testing functionality like
		// state-sunc where we might want to start some validators during tests.
		if _, ok := chainConfig.skipRunValidatorIndexes[i]; ok {
			s.T().Logf("skipping %s validator with index %d from running...", val.Name, i)
			continue
		}

		runOpts := &dockertest.RunOptions{
			Name:      val.Name,
			NetworkID: s.dkrNet.Network.ID,
			Mounts: []string{
				fmt.Sprintf("%s/:/osmosis/.osmosisd", val.ConfigDir),
				fmt.Sprintf("%s/scripts:/osmosis", pwd),
			},
			Repository: dockerRepository,
			Tag:        dockerTag,
			Cmd: []string{
				"start",
			},
		}

		// expose the first validator for debugging and communication
		if val.Index == 0 {
			runOpts.PortBindings = map[docker.Port][]docker.PortBinding{
				"1317/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 1317+portOffset)}},
				"6060/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6060+portOffset)}},
				"6061/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6061+portOffset)}},
				"6062/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6062+portOffset)}},
				"6063/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6063+portOffset)}},
				"6064/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6064+portOffset)}},
				"6065/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 6065+portOffset)}},
				"9090/tcp":  {{HostIP: "", HostPort: fmt.Sprintf("%d", 9090+portOffset)}},
				"26656/tcp": {{HostIP: "", HostPort: fmt.Sprintf("%d", 26656+portOffset)}},
				"26657/tcp": {{HostIP: "", HostPort: fmt.Sprintf("%d", 26657+portOffset)}},
			}
		}

		resource, err := s.dkrPool.RunWithOptions(runOpts, noRestart)
		s.Require().NoError(err)

		s.valResources[chain.ChainMeta.Id][i] = resource
		s.T().Logf("started %s validator container: %s", resource.Container.Name[1:], resource.Container.ID)
	}

	rpcClient, err := rpchttp.New("tcp://localhost:26657", "/websocket")
	s.Require().NoError(err)

	s.Require().Eventually(
		func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()

			status, err := rpcClient.Status(ctx)
			if err != nil {
				return false
			}

			// let the node produce a few blocks
			if status.SyncInfo.CatchingUp || status.SyncInfo.LatestBlockHeight < 3 {
				return false
			}

			return true
		},
		5*time.Minute,
		time.Second,
		"Osmosis node failed to produce blocks",
	)
}

func (s *IntegrationTestSuite) runIBCRelayer(chainA *chain.Chain, chainB *chain.Chain) {
	s.T().Log("starting Hermes relayer container...")

	tmpDir, err := ioutil.TempDir("", "osmosis-e2e-testnet-hermes-")
	s.Require().NoError(err)
	s.tmpDirs = append(s.tmpDirs, tmpDir)

	osmoAVal := chainA.Validators[0]
	osmoBVal := chainB.Validators[0]
	hermesCfgPath := path.Join(tmpDir, "hermes")

	s.Require().NoError(os.MkdirAll(hermesCfgPath, 0o755))
	_, err = util.CopyFile(
		filepath.Join("./scripts/", "hermes_bootstrap.sh"),
		filepath.Join(hermesCfgPath, "hermes_bootstrap.sh"),
	)
	s.Require().NoError(err)

	s.hermesResource, err = s.dkrPool.RunWithOptions(
		&dockertest.RunOptions{
			Name:       fmt.Sprintf("%s-%s-relayer", chainA.ChainMeta.Id, chainB.ChainMeta.Id),
			Repository: s.dockerImages.RelayerRepository,
			Tag:        s.dockerImages.RelayerTag,
			NetworkID:  s.dkrNet.Network.ID,
			Cmd: []string{
				"start",
			},
			User: "root:root",
			Mounts: []string{
				fmt.Sprintf("%s/:/root/hermes", hermesCfgPath),
			},
			ExposedPorts: []string{
				"3031",
			},
			PortBindings: map[docker.Port][]docker.PortBinding{
				"3031/tcp": {{HostIP: "", HostPort: "3031"}},
			},
			Env: []string{
				fmt.Sprintf("OSMO_A_E2E_CHAIN_ID=%s", chainA.ChainMeta.Id),
				fmt.Sprintf("OSMO_B_E2E_CHAIN_ID=%s", chainB.ChainMeta.Id),
				fmt.Sprintf("OSMO_A_E2E_VAL_MNEMONIC=%s", osmoAVal.Mnemonic),
				fmt.Sprintf("OSMO_B_E2E_VAL_MNEMONIC=%s", osmoBVal.Mnemonic),
				fmt.Sprintf("OSMO_A_E2E_VAL_HOST=%s", s.valResources[chainA.ChainMeta.Id][0].Container.Name[1:]),
				fmt.Sprintf("OSMO_B_E2E_VAL_HOST=%s", s.valResources[chainB.ChainMeta.Id][0].Container.Name[1:]),
			},
			Entrypoint: []string{
				"sh",
				"-c",
				"chmod +x /root/hermes/hermes_bootstrap.sh && /root/hermes/hermes_bootstrap.sh",
			},
		},
		noRestart,
	)
	s.Require().NoError(err)

	endpoint := fmt.Sprintf("http://%s/state", s.hermesResource.GetHostPort("3031/tcp"))
	s.Require().Eventually(
		func() bool {
			resp, err := http.Get(endpoint)
			if err != nil {
				return false
			}

			defer resp.Body.Close()

			bz, err := io.ReadAll(resp.Body)
			if err != nil {
				return false
			}

			var respBody map[string]interface{}
			if err := json.Unmarshal(bz, &respBody); err != nil {
				return false
			}

			status := respBody["status"].(string)
			result := respBody["result"].(map[string]interface{})

			return status == "success" && len(result["chains"].([]interface{})) == 2
		},
		5*time.Minute,
		time.Second,
		"hermes relayer not healthy",
	)

	s.T().Logf("started Hermes relayer container: %s", s.hermesResource.Container.ID)

	// XXX: Give time to both networks to start, otherwise we might see gRPC
	// transport errors.
	time.Sleep(10 * time.Second)

	// create the client, connection and channel between the two Osmosis chains
	s.connectIBCChains(chainA, chainB)
}

func (s *IntegrationTestSuite) configureChain(chainId string, validatorConfigs []*chain.ValidatorConfig, skipValidatorIndexes map[int]struct{}) {
	s.T().Logf("starting e2e infrastructure for chain-id: %s", chainId)
	tmpDir, err := ioutil.TempDir("", "osmosis-e2e-testnet-")

	s.T().Logf("temp directory for chain-id %v: %v", chainId, tmpDir)
	s.Require().NoError(err)

	validatorConfigBytes, err := json.Marshal(validatorConfigs)
	s.Require().NoError(err)

	numVal := float32(len(validatorConfigs))

	newChainConfig := chainConfig{
		votingPeriod:            propDepositBlocks + numVal*propVoteBlocks + propBufferBlocks,
		skipRunValidatorIndexes: skipValidatorIndexes,
	}

	votingPeriodDuration := time.Duration(int(newChainConfig.votingPeriod) * 1000000000)

	s.initResource, err = s.dkrPool.RunWithOptions(
		&dockertest.RunOptions{
			Name:       fmt.Sprintf("%s", chainId),
			Repository: s.dockerImages.InitRepository,
			Tag:        s.dockerImages.InitTag,
			NetworkID:  s.dkrNet.Network.ID,
			Cmd: []string{
				fmt.Sprintf("--data-dir=%s", tmpDir),
				fmt.Sprintf("--chain-id=%s", chainId),
				fmt.Sprintf("--config=%s", validatorConfigBytes),
				fmt.Sprintf("--voting-period=%v", votingPeriodDuration),
			},
			User: "root:root",
			Mounts: []string{
				fmt.Sprintf("%s:%s", tmpDir, tmpDir),
			},
		},
		noRestart,
	)
	s.Require().NoError(err)

	fileName := fmt.Sprintf("%v/%v-encode", tmpDir, chainId)
	s.T().Logf("serialized init file for chain-id %v: %v", chainId, fileName)

	// loop through the reading and unmarshaling of the init file a total of maxRetries or until error is nil
	// without this, test attempts to unmarshal file before docker container is finished writing
	for i := 0; i < maxRetries; i++ {
		encJson, _ := os.ReadFile(fileName)
		err = json.Unmarshal(encJson, &newChainConfig.chain)
		if err == nil {
			break
		}

		if i == maxRetries-1 {
			s.Require().NoError(err)
		}

		if i > 0 {
			time.Sleep(1 * time.Second)
		}
	}
	s.Require().NoError(s.dkrPool.Purge(s.initResource))

	s.chainConfigs = append(s.chainConfigs, &newChainConfig)
}

func (s *IntegrationTestSuite) configureDockerResources(chainIDOne, chainIDTwo string) {
	var err error
	s.dkrPool, err = dockertest.NewPool("")
	s.Require().NoError(err)

	s.dkrNet, err = s.dkrPool.CreateNetwork(fmt.Sprintf("%s-%s-testnet", chainIDOne, chainIDTwo))
	s.Require().NoError(err)

	s.valResources = make(map[string][]*dockertest.Resource)
}

func noRestart(config *docker.HostConfig) {
	// in this case we don't want the nodes to restart on failure
	config.RestartPolicy = docker.RestartPolicy{
		Name: "no",
	}
}

func (s *IntegrationTestSuite) upgrade() {
	// submit, deposit, and vote for upgrade proposal
	// prop height = current height + voting period + time it takes to submit proposal + small buffer
	for _, chainConfig := range s.chainConfigs {
		currentHeight := s.getCurrentChainHeight(s.valResources[chainConfig.chain.ChainMeta.Id][0].Container.ID)
		chainConfig.propHeight = currentHeight + int(chainConfig.votingPeriod) + int(propSubmitBlocks) + int(propBufferBlocks)
		s.submitProposal(chainConfig.chain, chainConfig.propHeight)
		s.depositProposal(chainConfig.chain)
		s.voteProposal(chainConfig)
	}

	// wait till all chains halt at upgrade height
	for _, chainConfig := range s.chainConfigs {
		curChain := chainConfig.chain

		for i := range chainConfig.chain.Validators {
			if _, ok := chainConfig.skipRunValidatorIndexes[i]; ok {
				continue
			}

			// use counter to ensure no new blocks are being created
			counter := 0
			s.T().Logf("waiting to reach upgrade height on %s validator container: %s", s.valResources[curChain.ChainMeta.Id][i].Container.Name[1:], s.valResources[curChain.ChainMeta.Id][i].Container.ID)
			s.Require().Eventually(
				func() bool {
					currentHeight := s.getCurrentChainHeight(s.valResources[curChain.ChainMeta.Id][i].Container.ID)
					if currentHeight != chainConfig.propHeight {
						s.T().Logf("current block height on %s is %v, waiting for block %v container: %s", s.valResources[curChain.ChainMeta.Id][i].Container.Name[1:], currentHeight, chainConfig.propHeight, s.valResources[curChain.ChainMeta.Id][i].Container.ID)
					}
					if currentHeight > chainConfig.propHeight {
						panic("chain did not halt at upgrade height")
					}
					if currentHeight == chainConfig.propHeight {
						counter++
					}
					return counter == 3
				},
				5*time.Minute,
				time.Second,
			)
			s.T().Logf("reached upgrade height on %s container: %s", s.valResources[curChain.ChainMeta.Id][i].Container.Name[1:], s.valResources[curChain.ChainMeta.Id][i].Container.ID)
		}
	}

	// remove all containers so we can upgrade them to the new version
	for _, chainConfig := range s.chainConfigs {
		curChain := chainConfig.chain
		for valIdx := range curChain.Validators {
			if _, ok := chainConfig.skipRunValidatorIndexes[valIdx]; ok {
				continue
			}

			var opts docker.RemoveContainerOptions
			opts.ID = s.valResources[curChain.ChainMeta.Id][valIdx].Container.ID
			opts.Force = true
			s.dkrPool.Client.RemoveContainer(opts)
			s.T().Logf("removed container: %s", s.valResources[curChain.ChainMeta.Id][valIdx].Container.Name[1:])
		}
	}

	// remove all containers so we can upgrade them to the new version
	for _, chainConfig := range s.chainConfigs {
		s.upgradeContainers(chainConfig, chainConfig.propHeight)
	}
}

func (s *IntegrationTestSuite) upgradeContainers(chainConfig *chainConfig, propHeight int) {
	// upgrade containers to the locally compiled daemon
	chain := chainConfig.chain
	s.T().Logf("starting upgrade for chain-id: %s...", chain.ChainMeta.Id)
	pwd, err := os.Getwd()
	s.Require().NoError(err)

	for i, val := range chain.Validators {
		if _, ok := chainConfig.skipRunValidatorIndexes[i]; ok {
			continue
		}

		runOpts := &dockertest.RunOptions{
			Name:       val.Name,
			Repository: dockerconfig.LocalOsmoRepository,
			Tag:        dockerconfig.LocalOsmoTag,
			NetworkID:  s.dkrNet.Network.ID,
			User:       "root:root",
			Mounts: []string{
				fmt.Sprintf("%s/:/osmosis/.osmosisd", val.ConfigDir),
				fmt.Sprintf("%s/scripts:/osmosis", pwd),
			},
		}
		resource, err := s.dkrPool.RunWithOptions(runOpts, noRestart)
		s.Require().NoError(err)

		s.valResources[chain.ChainMeta.Id][i] = resource
		s.T().Logf("started %s validator container: %s", resource.Container.Name[1:], resource.Container.ID)
	}

	// check that we are creating blocks again
	for i := range chain.Validators {
		if _, ok := chainConfig.skipRunValidatorIndexes[i]; ok {
			continue
		}

		s.Require().Eventually(
			func() bool {
				currentHeight := s.getCurrentChainHeight(s.valResources[chain.ChainMeta.Id][i].Container.ID)
				if currentHeight <= propHeight {
					s.T().Logf("current block height on %s is %v, waiting to create blocks container: %s", s.valResources[chain.ChainMeta.Id][i].Container.Name[1:], currentHeight, s.valResources[chain.ChainMeta.Id][i].Container.ID)
				}
				return currentHeight > propHeight
			},
			5*time.Minute,
			time.Second,
		)
		s.T().Logf("upgrade successful on %s validator container: %s", s.valResources[chain.ChainMeta.Id][i].Container.Name[1:], s.valResources[chain.ChainMeta.Id][i].Container.ID)
	}
}

func (s *IntegrationTestSuite) createPreUpgradeState() {
	chainA := s.chainConfigs[0].chain
	chainB := s.chainConfigs[1].chain

	s.sendIBC(chainA, chainB, chainB.Validators[0].PublicAddress, chain.OsmoToken)
	s.sendIBC(chainB, chainA, chainA.Validators[0].PublicAddress, chain.OsmoToken)
	s.sendIBC(chainA, chainB, chainB.Validators[0].PublicAddress, chain.StakeToken)
	s.sendIBC(chainB, chainA, chainA.Validators[0].PublicAddress, chain.StakeToken)
	s.createPool(chainA, "pool1A.json")
	s.createPool(chainB, "pool1B.json")
}

func (s *IntegrationTestSuite) runPostUpgradeTests() {
	chainA := s.chainConfigs[0].chain
	chainB := s.chainConfigs[1].chain

	s.sendIBC(chainA, chainB, chainB.Validators[0].PublicAddress, chain.OsmoToken)
	s.sendIBC(chainB, chainA, chainA.Validators[0].PublicAddress, chain.OsmoToken)
	s.sendIBC(chainA, chainB, chainB.Validators[0].PublicAddress, chain.StakeToken)
	s.sendIBC(chainB, chainA, chainA.Validators[0].PublicAddress, chain.StakeToken)
	s.createPool(chainA, "pool2A.json")
	s.createPool(chainB, "pool2B.json")
}

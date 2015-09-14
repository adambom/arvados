/* Deals with getting Keep Server blocks from API Server and Keep Servers. */

package keep

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/blockdigest"
	"git.curoverse.com/arvados.git/sdk/go/keepclient"
	"git.curoverse.com/arvados.git/sdk/go/logger"
	"git.curoverse.com/arvados.git/services/datamanager/loggerutil"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServerAddress struct
type ServerAddress struct {
	SSL  bool   `json:service_ssl_flag`
	Host string `json:"service_host"`
	Port int    `json:"service_port"`
	UUID string `json:"uuid"`
}

// BlockInfo is info about a particular block returned by the server
type BlockInfo struct {
	Digest blockdigest.DigestWithSize
	Mtime  int64 // TODO(misha): Replace this with a timestamp.
}

// BlockServerInfo is info about a specified block given by a server
type BlockServerInfo struct {
	ServerIndex int
	Mtime       int64 // TODO(misha): Replace this with a timestamp.
}

// ServerContents struct
type ServerContents struct {
	BlockDigestToInfo map[blockdigest.DigestWithSize]BlockInfo
}

// ServerResponse struct
type ServerResponse struct {
	Address  ServerAddress
	Contents ServerContents
}

// ReadServers struct
type ReadServers struct {
	ReadAllServers           bool
	KeepServerIndexToAddress []ServerAddress
	KeepServerAddressToIndex map[ServerAddress]int
	ServerToContents         map[ServerAddress]ServerContents
	BlockToServers           map[blockdigest.DigestWithSize][]BlockServerInfo
	BlockReplicationCounts   map[int]int
}

// GetKeepServersParams struct
type GetKeepServersParams struct {
	Client arvadosclient.ArvadosClient
	Logger *logger.Logger
	Limit  int
}

// ServiceList consists of the addresses of all the available kee servers
type ServiceList struct {
	ItemsAvailable int             `json:"items_available"`
	KeepServers    []ServerAddress `json:"items"`
}

// String 
// TODO(misha): Change this to include the UUID as well.
func (s ServerAddress) String() string {
	return s.URL()
}

// URL of the keep server
func (s ServerAddress) URL() string {
	if s.SSL {
		return fmt.Sprintf("https://%s:%d", s.Host, s.Port)
	}
	return fmt.Sprintf("http://%s:%d", s.Host, s.Port)
}

// GetKeepServersAndSummarize gets keep servers from api
func GetKeepServersAndSummarize(params GetKeepServersParams) (results ReadServers) {
	results = GetKeepServers(params)
	log.Printf("Returned %d keep disks", len(results.ServerToContents))

	results.Summarize(params.Logger)
	log.Printf("Replication level distribution: %v",
		results.BlockReplicationCounts)

	return
}

// GetKeepServers from api server
func GetKeepServers(params GetKeepServersParams) (results ReadServers) {
	sdkParams := arvadosclient.Dict{
		"filters": [][]string{[]string{"service_type", "=", "disk"}},
	}
	if params.Limit > 0 {
		sdkParams["limit"] = params.Limit
	}

	var sdkResponse ServiceList
	err := params.Client.List("keep_services", sdkParams, &sdkResponse)

	if err != nil {
		loggerutil.FatalWithMessage(params.Logger,
			fmt.Sprintf("Error requesting keep disks from API server: %v", err))
	}

	if params.Logger != nil {
		params.Logger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			keepInfo["num_keep_servers_available"] = sdkResponse.ItemsAvailable
			keepInfo["num_keep_servers_received"] = len(sdkResponse.KeepServers)
			keepInfo["keep_servers"] = sdkResponse.KeepServers
		})
	}

	log.Printf("Received keep services list: %+v", sdkResponse)

	if len(sdkResponse.KeepServers) < sdkResponse.ItemsAvailable {
		loggerutil.FatalWithMessage(params.Logger,
			fmt.Sprintf("Did not receive all available keep servers: %+v", sdkResponse))
	}

	results.KeepServerIndexToAddress = sdkResponse.KeepServers
	results.KeepServerAddressToIndex = make(map[ServerAddress]int)
	for i, address := range results.KeepServerIndexToAddress {
		results.KeepServerAddressToIndex[address] = i
	}

	log.Printf("Got Server Addresses: %v", results)

	// Send off all the index requests concurrently
	responseChan := make(chan ServerResponse)
	for _, keepServer := range sdkResponse.KeepServers {
		// The above keepsServer variable is reused for each iteration, so
		// it would be shared across all goroutines. This would result in
		// us querying one server n times instead of n different servers
		// as we intended. To avoid this we add it as an explicit
		// parameter which gets copied. This bug and solution is described
		// in https://golang.org/doc/effective_go.html#channels
		go func(keepServer ServerAddress) {
			responseChan <- GetServerContents(params.Logger,
				keepServer,
				params.Client)
		}(keepServer)
	}

	results.ServerToContents = make(map[ServerAddress]ServerContents)
	results.BlockToServers = make(map[blockdigest.DigestWithSize][]BlockServerInfo)

	// Read all the responses
	for i := range sdkResponse.KeepServers {
		_ = i // Here to prevent go from complaining.
		response := <-responseChan
		log.Printf("Received channel response from %v containing %d files",
			response.Address,
			len(response.Contents.BlockDigestToInfo))
		results.ServerToContents[response.Address] = response.Contents
		serverIndex := results.KeepServerAddressToIndex[response.Address]
		for _, blockInfo := range response.Contents.BlockDigestToInfo {
			results.BlockToServers[blockInfo.Digest] = append(
				results.BlockToServers[blockInfo.Digest],
				BlockServerInfo{ServerIndex: serverIndex,
					Mtime: blockInfo.Mtime})
		}
	}
	return
}

// GetServerContents of the keep server
func GetServerContents(arvLogger *logger.Logger,
	keepServer ServerAddress,
	arv arvadosclient.ArvadosClient) (response ServerResponse) {

	GetServerStatus(arvLogger, keepServer, arv)

	req := CreateIndexRequest(arvLogger, keepServer, arv)
	resp, err := arv.Client.Do(req)
	if err != nil {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Error fetching %s: %v. Response was %+v",
				req.URL.String(),
				err,
				resp))
	}

	return ReadServerResponse(arvLogger, keepServer, resp)
}

// GetServerStatus get keep server status by invoking /status.json
func GetServerStatus(arvLogger *logger.Logger,
	keepServer ServerAddress,
	arv arvadosclient.ArvadosClient) {
	url := fmt.Sprintf("http://%s:%d/status.json",
		keepServer.Host,
		keepServer.Port)

	if arvLogger != nil {
		now := time.Now()
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			serverInfo := make(map[string]interface{})
			serverInfo["status_request_sent_at"] = now
			serverInfo["host"] = keepServer.Host
			serverInfo["port"] = keepServer.Port

			keepInfo[keepServer.UUID] = serverInfo
		})
	}

	resp, err := arv.Client.Get(url)
	if err != nil {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Error getting keep status from %s: %v", url, err))
	} else if resp.StatusCode != 200 {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Received error code %d in response to request "+
				"for %s status: %s",
				resp.StatusCode, url, resp.Status))
	}

	var keepStatus map[string]interface{}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	err = decoder.Decode(&keepStatus)
	if err != nil {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Error decoding keep status from %s: %v", url, err))
	}

	if arvLogger != nil {
		now := time.Now()
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			serverInfo := keepInfo[keepServer.UUID].(map[string]interface{})
			serverInfo["status_response_processed_at"] = now
			serverInfo["status"] = keepStatus
		})
	}
}

// CreateIndexRequest to the keep server
func CreateIndexRequest(arvLogger *logger.Logger,
	keepServer ServerAddress,
	arv arvadosclient.ArvadosClient) (req *http.Request) {
	url := fmt.Sprintf("http://%s:%d/index", keepServer.Host, keepServer.Port)
	log.Println("About to fetch keep server contents from " + url)

	if arvLogger != nil {
		now := time.Now()
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			serverInfo := keepInfo[keepServer.UUID].(map[string]interface{})
			serverInfo["index_request_sent_at"] = now
		})
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Error building http request for %s: %v", url, err))
	}

	req.Header.Add("Authorization", "OAuth2 " + arv.ApiToken)
	return
}

// ReadServerResponse reads reasponse from keep server
func ReadServerResponse(arvLogger *logger.Logger,
	keepServer ServerAddress,
	resp *http.Response) (response ServerResponse) {

	if resp.StatusCode != 200 {
		loggerutil.FatalWithMessage(arvLogger,
			fmt.Sprintf("Received error code %d in response to request "+
				"for %s index: %s",
				resp.StatusCode, keepServer.String(), resp.Status))
	}

	if arvLogger != nil {
		now := time.Now()
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			serverInfo := keepInfo[keepServer.UUID].(map[string]interface{})
			serverInfo["index_response_received_at"] = now
		})
	}

	response.Address = keepServer
	response.Contents.BlockDigestToInfo =
		make(map[blockdigest.DigestWithSize]BlockInfo)
	reader := bufio.NewReader(resp.Body)
	numLines, numDuplicates, numSizeDisagreements := 0, 0, 0
	for {
		numLines++
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			loggerutil.FatalWithMessage(arvLogger,
				fmt.Sprintf("Index from %s truncated at line %d",
					keepServer.String(), numLines))
		} else if err != nil {
			loggerutil.FatalWithMessage(arvLogger,
				fmt.Sprintf("Error reading index response from %s at line %d: %v",
					keepServer.String(), numLines, err))
		}
		if line == "\n" {
			if _, err := reader.Peek(1); err == nil {
				extra, _ := reader.ReadString('\n')
				loggerutil.FatalWithMessage(arvLogger,
					fmt.Sprintf("Index from %s had trailing data at line %d after EOF marker: %s",
						keepServer.String(), numLines+1, extra))
			} else if err != io.EOF {
				loggerutil.FatalWithMessage(arvLogger,
					fmt.Sprintf("Index from %s had read error after EOF marker at line %d: %v",
						keepServer.String(), numLines, err))
			}
			numLines--
			break
		}
		blockInfo, err := parseBlockInfoFromIndexLine(line)
		if err != nil {
			loggerutil.FatalWithMessage(arvLogger,
				fmt.Sprintf("Error parsing BlockInfo from index line "+
					"received from %s: %v",
					keepServer.String(),
					err))
		}

		if storedBlock, ok := response.Contents.BlockDigestToInfo[blockInfo.Digest]; ok {
			// This server returned multiple lines containing the same block digest.
			numDuplicates++
			// Keep the block that's newer.
			if storedBlock.Mtime < blockInfo.Mtime {
				response.Contents.BlockDigestToInfo[blockInfo.Digest] = blockInfo
			}
		} else {
			response.Contents.BlockDigestToInfo[blockInfo.Digest] = blockInfo
		}
	}

	log.Printf("%s index contained %d lines with %d duplicates with "+
		"%d size disagreements",
		keepServer.String(),
		numLines,
		numDuplicates,
		numSizeDisagreements)

	if arvLogger != nil {
		now := time.Now()
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			serverInfo := keepInfo[keepServer.UUID].(map[string]interface{})

			serverInfo["processing_finished_at"] = now
			serverInfo["lines_received"] = numLines
			serverInfo["duplicates_seen"] = numDuplicates
			serverInfo["size_disagreements_seen"] = numSizeDisagreements
		})
	}
	resp.Body.Close()
	return
}

func parseBlockInfoFromIndexLine(indexLine string) (blockInfo BlockInfo, err error) {
	tokens := strings.Fields(indexLine)
	if len(tokens) != 2 {
		err = fmt.Errorf("Expected 2 tokens per line but received a "+
			"line containing %v instead.",
			tokens)
	}

	var locator blockdigest.BlockLocator
	if locator, err = blockdigest.ParseBlockLocator(tokens[0]); err != nil {
		err = fmt.Errorf("%v Received error while parsing line \"%s\"",
			err, indexLine)
		return
	}
	if len(locator.Hints) > 0 {
		err = fmt.Errorf("Block locator in index line should not contain hints "+
			"but it does: %v",
			locator)
		return
	}

	blockInfo.Mtime, err = strconv.ParseInt(tokens[1], 10, 64)
	if err != nil {
		return
	}
	blockInfo.Digest =
		blockdigest.DigestWithSize{Digest: locator.Digest,
			Size: uint32(locator.Size)}
	return
}

// Summarize results from keep server
func (readServers *ReadServers) Summarize(arvLogger *logger.Logger) {
	readServers.BlockReplicationCounts = make(map[int]int)
	for _, infos := range readServers.BlockToServers {
		replication := len(infos)
		readServers.BlockReplicationCounts[replication]++
	}

	if arvLogger != nil {
		arvLogger.Update(func(p map[string]interface{}, e map[string]interface{}) {
			keepInfo := logger.GetOrCreateMap(p, "keep_info")
			keepInfo["distinct_blocks_stored"] = len(readServers.BlockToServers)
		})
	}
}

// TrashRequest struct
type TrashRequest struct {
	Locator    string `json:"locator"`
	BlockMtime int64  `json:"block_mtime"`
}

// TrashList is an array of TrashRequest objects
type TrashList []TrashRequest

// SendTrashLists to trash queue
func SendTrashLists(kc *keepclient.KeepClient, spl map[string]TrashList) (errs []error) {
	count := 0
	barrier := make(chan error)

	client := kc.Client

	for url, v := range spl {
		count++
		log.Printf("Sending trash list to %v", url)

		go (func(url string, v TrashList) {
			pipeReader, pipeWriter := io.Pipe()
			go (func() {
				enc := json.NewEncoder(pipeWriter)
				enc.Encode(v)
				pipeWriter.Close()
			})()

			req, err := http.NewRequest("PUT", fmt.Sprintf("%s/trash", url), pipeReader)
			if err != nil {
				log.Printf("Error creating trash list request for %v error: %v", url, err.Error())
				barrier <- err
				return
			}

			req.Header.Add("Authorization", "OAuth2 " + kc.Arvados.ApiToken)

			// Make the request
			var resp *http.Response
			if resp, err = client.Do(req); err != nil {
				log.Printf("Error sending trash list to %v error: %v", url, err.Error())
				barrier <- err
				return
			}

			log.Printf("Sent trash list to %v: response was HTTP %v", url, resp.Status)

			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()

			if resp.StatusCode != 200 {
				barrier <- errors.New(fmt.Sprintf("Got HTTP code %v", resp.StatusCode))
			} else {
				barrier <- nil
			}
		})(url, v)

	}

	for i := 0; i < count; i++ {
		b := <-barrier
		if b != nil {
			errs = append(errs, b)
		}
	}

	return errs
}

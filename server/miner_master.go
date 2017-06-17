package main

import (
    "fmt"
    "log"
    "time"
    "strings"
    "sync"

    pb "../protobuf/go"
    "github.com/golang/protobuf/jsonpb"
)

type OnReceiveResponse struct {
    c chan bool
}

func NewOnReceiveResponse() *OnReceiveResponse {
    return &OnReceiveResponse{
        c: make(chan bool, 1),
    }
}

func (r *OnReceiveResponse) Finish() {
    r.c <- true
}

func (r *OnReceiveResponse) Wait() bool {
    rc := <-r.c
    close(r.c)
    return rc
}

type MinerMaster interface {
    Recover() error
    Mainloop()

    // Client-side 
    GetUserInfo(uid string) *UserInfo
    GetLatestBlock() *BlockInfo
    // transaction => return code, blockhash
    VerifyClientTransaction(t *pb.Transaction) (int, string)
    OnClientTransactionAsync(t *pb.Transaction) bool

    // Peer-side
    GetBlock(bid string) *BlockInfo
    OnBlockAsync(json string)
    OnTransactionAsync(t *pb.Transaction)

    OnWorkerSuccess(json string)
}

func NewMinerMaster(c *ServerConfig) (master MinerMaster, e error) {
    p2pc := NewP2PClient(c)

    switch c.Miner.MinerType {
    case "Honest":
        m := &HonestMinerMaster{
            BaseMinerMaster: BaseMinerMaster{
                BC: NewBlockChain(c, p2pc),
                P2PC: p2pc,
                jsonMarshaler: &jsonpb.Marshaler{EnumsAsInts: false},
            },
            config: c,
            workers: make([]MinerWorker, 0),
            updateMutex: &sync.Mutex{},
        }

        // Start workers
        for i := 0; i < m.config.Miner.NrWorkers; i++ {
            log.Printf("Starting worker #%d", i)
            w := NewSimpleMinerWorker(m)
            m.workers = append(m.workers, w)
            go w.Mainloop()
        }

        master = m

    default:
        e = fmt.Errorf("Invalid miner type: %s", c.Miner.MinerType)
    }
    return
}

type BaseMinerMaster struct {
    BC  *BlockChain
    P2PC *P2PClient

    jsonMarshaler *jsonpb.Marshaler
}

func (m *BaseMinerMaster) GetUserInfo(uid string) *UserInfo {
    return m.BC.GetUserInfo(uid)
}

func (m *BaseMinerMaster) GetLatestBlock() *BlockInfo {
    return m.BC.GetLatestBlock()
}

func (m *BaseMinerMaster) VerifyClientTransaction(t *pb.Transaction) (rc int, hash string) {
    rc, hash = m.BC.VerifyTransaction6(t)
    return
}

type HonestMinerMaster struct {
    BaseMinerMaster

    config   *ServerConfig
    workers []MinerWorker
    updateMutex *sync.Mutex
}

func (m *HonestMinerMaster) Recover() (err error) {
    // TODO:: 
    return nil
}

func (m *HonestMinerMaster) Mainloop() {
    for {
        // Mainloop here
        time.Sleep(1000)
    }
}

func (m *HonestMinerMaster) GetBlock(bid string) *BlockInfo {
    b, ok := m.BC.GetBlock(bid)
    if !ok {
        return nil
    }
    return b
}

func (m *HonestMinerMaster) OnClientTransactionAsync(t *pb.Transaction) bool {
    // Broadcast::
    if true {
        res := m.P2PC.RemotePushTransactionAsync(t)
        _ = res.Get()
        res.IgnoreLater()
    }
    return m.processTransaction(t)
}

func (m *HonestMinerMaster) OnTransactionAsync(t *pb.Transaction) {
    _ = m.processTransaction(t)
}

func (m *HonestMinerMaster) OnBlockAsync(json string) {
    lastChanged, _ := m.BC.PushBlockJson(json)
    if lastChanged {
        // TODO:: Accelerate the selection
        // First, test whether the current working block is valid or not.
        m.updateWorkingSet(true)
    }
}

func (m *HonestMinerMaster) OnWorkerSuccess(json string) {
    log.Printf("On worker success: %s.", json)
    _, err := m.BC.DeclareBlockJson(json)
    if err == nil {
        _ = m.P2PC.RemotePushBlockAsync(json)
        m.updateWorkingSet(true)
    } else {
        log.Printf("Got invalid declaration: %s.", json)
    }
}

func (m *HonestMinerMaster) processTransaction(t *pb.Transaction) bool {
    // TODO:: Flow control

    rc := m.BC.PushTransaction(t, true)
    if rc == 0 || rc == 2 {
        return false
    }

    // Passed check
    m.updateWorkingSet(false)

    return true
}

func (m *HonestMinerMaster) updateWorkingSet(forceUpdate bool) {
    m.updateMutex.Lock()
    defer m.updateMutex.Unlock()

    if !forceUpdate {
        // Check non-working workers first
        flag := false
        for _, w := range m.workers {
            if !w.Working() {
                flag = true
                break
            }
        }

        if !flag {
            return
        }
    }

    st := NewBlockChainTStack(m.BC, true, true)
    defer st.Close()

    validTransactions := make([]*pb.Transaction, 0)

    nrProcessed := 0
    for _, trans := range m.BC.PendingTransactions {

        if st.TestAndDo(trans) {
            validTransactions = append(validTransactions, trans)
        }
        nrProcessed += 1

        // TODO:: config: 100, 50
        if (len(validTransactions) > 0 && nrProcessed > 100) || len(validTransactions) == 50 {
            break
        }
    }

    if len(validTransactions) == 0 {
        return
    }

    // Note:: here, we have BlockMutex.R, UserMutex.R
    block := &pb.Block{
        BlockID: m.BC.LatestBlock.Block.BlockID + 1,
        PrevHash: m.BC.LatestBlock.Hash,
        Transactions: validTransactions,
        MinerID: m.config.Self.ID,
        Nonce: "00000000",
    }

    json, err := m.jsonMarshaler.MarshalToString(block)
    if err != nil {
        return
    }

    presuf := strings.Split(json, "\"Nonce\":\"00000000\"")
    // Sanity check
    if len(presuf) != 2 {
        return
    }

    log.Printf("Updating working set: %s.", json)

    prefix, suffix := presuf[0], presuf[1]
    prefix = prefix + "\"Nonce\":\""
    suffix = "\"" + suffix
    for _, w := range m.workers {
        if !w.Working() || forceUpdate {
            w.UpdateWorkingBlock(prefix, suffix)
        }
    }
}

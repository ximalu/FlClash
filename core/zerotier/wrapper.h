#pragma once
// FlClashTier M1: thin C ABI wrapper around ZeroTier C API.
// Go talks to this C ABI only; never to ZeroTier C++ ABI directly.
//
// Layout:
//   core/zerotier/wrapper.h            <- this header (C ABI)
//   core/zerotier/cpp_src/wrapper.cpp  <- C++ implementation (precompiled to
//                                         wrapper_android.o by build_tool /
//                                         wrapper_linux.o by the Linux test script)
//   core/zerotier/include/ZeroTierOne.h      (from ZeroTierOne 1.10.5, BSL 1.1)
//   core/zerotier/planet_data.h              (official planet bytes, xxd -i)
//   core/zerotier/lib/libzerotiercore-{android,linux}.a
//
// Build tags on the Go side:
//   zt_android.go  //go:build android && cgo   LDFLAGS -> android .a
//   zt_linux.go    //go:build linux && !android LDFLAGS -> linux .a
#include <stdint.h>
#include <sys/socket.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque handle to a ZeroTier Node (maps to ZT_Node*).
typedef struct flclashtier_zt_node flclashtier_zt_node;

// Create a ZeroTier Node (registers all callbacks, wires the embedded planet
// and identity persistence). Returns NULL on failure.
flclashtier_zt_node* flclashtier_zt_node_new(void);

// Destroy a ZeroTier Node. NULL-safe.
void flclashtier_zt_node_delete(flclashtier_zt_node* node);

// ZeroTier library version string (major.minor.revision), caller must not free.
const char* flclashtier_zt_version(void);

// 换网后用：把 core 里所有 peer 物理路径标为不可用，并立刻经由当前 root 重新
// HELLO（= 冷启动那次重新打洞），但不销毁 node、identity、managed routes。
// family: 0 = 全部地址族, 4 = 仅 IPv4, 6 = 仅 IPv6。返回 0 表示成功。
// 只存在于打过 FlClashTier 补丁的 libzerotiercore（ZT_Node_resetPeerPaths）。
int flclashtier_zt_reset_peer_paths(int family);

// 换网后的**软**版本：给每个已记住的对端端点发一次探测（ECHO），但不清 lr、
// 不丢端点记忆。端口限制型 NAT 下硬 reset 会忘掉唯一能开洞的端点，然后卡到
// ZT_PEER_PATH_EXPIRATION（243 s）；软探测可以反复打（Go 侧爆发 6 次）。
// 返回发出的探测数，-1 = 没有运行中的 node。
int flclashtier_zt_reprobe_peer_paths(int family);

// 经 root 转发一次 HELLO（重新会合）：让对端学到我们的新公网端点。不清 lr、
// 不删 path。返回发出的 HELLO 数，-1 = 没有 node，0 = 还没有 root 路径。
int flclashtier_zt_rehello_peers(void);

// 内核补丁能力水位：1 = 只有硬 reset，2 = +软探测，3 = +经 root 转发 HELLO。启动时打一行日志，
// 避免「补丁没进包」只表现为稳定地 243 s 恢复。
int flclashtier_zt_patch_level(void);

// ZeroTier node address (40-bit, as uint64).
uint64_t flclashtier_zt_node_address(void);

// Set path used to persist the ZeroTier identity (secret). If unset, a new
// identity is generated on every start (node address changes). Call before
// flclashtier_zt_node_new().
void flclashtier_zt_set_identity_path(const char* path);

// ---------------------------------------------------------------------------
// Network config snapshot (routes / assigned addresses / status / MAC).
// C-side, thread-safe. Go pulls a copy via flclashtier_zt_snapshot_export().
// ---------------------------------------------------------------------------
#define FLCLASHTIER_ZT_MAX_ROUTES 128
#define FLCLASHTIER_ZT_MAX_ASSIGNED 16

typedef struct {
    uint8_t  family;     // 4 / 6 / 0 = none
    uint8_t  prefixLen;  // from sockaddr port field (netmask bits)
    uint8_t  addr[16];   // network order (4 or 16 bytes)
} flclashtier_zt_assigned;

typedef struct {
    uint8_t  family;     // 4 / 6 / 0 = none
    uint8_t  prefixLen;
    uint8_t  target[16]; // network address (network order)
    uint8_t  via[16];    // gateway (all-zero = direct)
    uint16_t flags;
    uint16_t metric;
} flclashtier_zt_route;

typedef struct {
    uint64_t nwid;
    int      operation;  // ZT_VirtualNetworkConfigOperation
    int      status;     // ZT_VirtualNetworkStatus (1 = OK)
    uint64_t netconfRevision;
    uint64_t mac;        // virtual MAC (lower 48 bits)
    uint32_t mtu;        // network MTU
    uint32_t assignedCount;
    flclashtier_zt_assigned assigned[FLCLASHTIER_ZT_MAX_ASSIGNED];
    uint32_t routeCount;
    flclashtier_zt_route routes[FLCLASHTIER_ZT_MAX_ROUTES];
} flclashtier_zt_snapshot;

// Export the latest snapshot into caller-provided memory (Go-owned buffer).
// Returns the snapshot generation (increments on every config event).
uint64_t flclashtier_zt_snapshot_export(flclashtier_zt_snapshot* out);

// ---------------------------------------------------------------------------
// Wire plane (M1-3)
// ---------------------------------------------------------------------------
// Go creates the UDP socket (so it can VpnService.protect() the fd on
// Android), then hands the raw fd to C. C holds only an int, not a Go
// pointer, so cgo pointer rules are satisfied.
void flclashtier_zt_set_socket_fd(int fd);
int  flclashtier_zt_get_socket_fd(void);

// Feed a received UDP payload (remoteAddress + data) into ZT_Node_processWirePacket.
// Returns ZT_ResultCode; nextDeadline is filled if non-NULL.
int flclashtier_zt_process_wire_packet(const struct sockaddr_storage* remoteAddr,
                                       const void* data, unsigned int len,
                                       int64_t now, int64_t* nextDeadline);

// Drive ZT_Node_processBackgroundTasks (timeouts/retries/path probes).
int flclashtier_zt_process_background_tasks(int64_t now, int64_t* nextDeadline);

// Join / leave a ZeroTier network.
int flclashtier_zt_join(uint64_t nwid);
int flclashtier_zt_leave(uint64_t nwid);

// ---------------------------------------------------------------------------
// Frame plane (M1)
// ---------------------------------------------------------------------------
#define FLCLASHTIER_ZT_FRAME_MAX 4096
#define FLCLASHTIER_ZT_FRAME_QUEUE 512

typedef struct {
    uint64_t nwid;
    uint64_t srcMac;
    uint64_t dstMac;
    uint16_t etherType;
    uint16_t vlanId;
    uint32_t len;
    uint8_t  data[FLCLASHTIER_ZT_FRAME_MAX];
} flclashtier_zt_frame;

// Pull the oldest received virtual frame. Returns 1 if a frame was copied,
// 0 if the queue is empty. Frames are dropped (oldest first) when the queue
// is full; see flclashtier_zt_frame_dropped().
int flclashtier_zt_frame_pull(flclashtier_zt_frame* out);

// Physical wire send statistics: total sendto() calls that succeeded, and the
// failures broken down by errno class. Without these the physical UDP send
// path is a black box (a full socket returns EAGAIN/ENOBUFS and the packet is
// silently gone).
// Capacity of the C-side received-frame ring queue (compile-time constant made
// visible to Go so tests and status can assert it).
int flclashtier_zt_frame_queue_capacity(void);

// sendto() errno histogram (index = errno, last slot = overflow).
void flclashtier_zt_send_errno_stats(uint64_t* out, int slots);
void flclashtier_zt_send_family_stats(uint64_t* v4ok, uint64_t* v4err,
                                      uint64_t* v6ok, uint64_t* v6err);
void flclashtier_zt_set_socket_fd6(int fd);
int flclashtier_zt_socket_fd6(void);
int flclashtier_zt_last_send_errno(void);

void flclashtier_zt_wire_send_stats(uint64_t* ok, uint64_t* eagain,
                                    uint64_t* enobufs, uint64_t* other);
// Bytes successfully handed to sendto() by the ZT wire layer (used by the
// network page RX/TX rates; proxy traffic never enters it).
void flclashtier_zt_wire_tx_bytes(uint64_t* out);
// Number of frames currently waiting in the RX queue.
int flclashtier_zt_frame_pending(void);
// Total frames dropped because the RX queue was full.
uint64_t flclashtier_zt_frame_dropped(void);

// Send a virtual network frame (wraps ZT_Node_processVirtualNetworkFrame).
// frameData is copied by the core during the call (transient Go pointer OK).
int flclashtier_zt_send_frame(uint64_t nwid, uint64_t srcMac, uint64_t dstMac,
                              uint16_t etherType, uint16_t vlanId,
                              const void* data, unsigned int len,
                              int64_t* nextDeadline);

// Multicast subscription (wraps ZT_Node_multicastSubscribe/Unsubscribe).
// adi is the multicast ADI. For IPv4 ARP resolution groups pass the queried
// IPv4 address as a big-endian uint32 (ZeroTier derives the group that way);
// for everything else pass 0.
int flclashtier_zt_multicast_subscribe(uint64_t nwid, uint64_t multicastGroup, uint32_t adi);
int flclashtier_zt_multicast_unsubscribe(uint64_t nwid, uint64_t multicastGroup, uint32_t adi);

// ---------------------------------------------------------------------------
// Peer plane (网络页「对端连接」)
// ---------------------------------------------------------------------------
// Point-in-time snapshot of ZT_Node_peers(), deep-copied into fixed-size C
// memory (no Go pointers, no pointers into the core's result buffer).
//
// ⚠️ Raw evidence only — NO direct/relay verdict:
//   * trustedPathId in the core's own ZT_PeerPhysicalPath is the LOCAL
//     trusted-path configuration for that endpoint (Node.cpp:
//     getOutboundPathTrust), it does not mark relayed paths.
//   * The core's own CLI prints RELAY only when its TCP fallback tunnel is up,
//     or as a fallback when no path is "preferred" (one.cpp) — and
//     "preferred" merely means the path is younger than
//     ZT_PEER_PATH_EXPIRATION, not that it is delivering traffic.
//   Also note: Node::peers() only copies paths passing Path::valid(), so
//   `expired` is always 0 here and expired paths never appear at all.
#define FLCLASHTIER_ZT_MAX_PEERS 64

typedef struct {
    uint64_t address;        // 40-bit node address
    int32_t  latencyMs;      // -1 = unknown
    int32_t  versionMajor;   // -1 = unknown
    int32_t  versionMinor;
    int32_t  versionRev;
    int32_t  role;           // ZT_PeerRole (0 LEAF / 1 MOON / 2 PLANET)
    uint32_t pathCount;      // non-expired paths the core reports
    uint32_t preferredPaths; // subset the core would currently send on
    uint32_t pathsWithRx;    // subset that received at least one packet
    uint64_t lastReceiveMs;  // newest path lastReceive (0 = never)
    uint64_t lastSendMs;     // newest path lastSend (0 = never)
} flclashtier_zt_peer;

typedef struct {
    int32_t ok;              // 1 = query ran, 0 = no live node / core returned NULL
    int32_t count;           // peers written (<= FLCLASHTIER_ZT_MAX_PEERS)
    int32_t truncated;       // 1 = the core reported more peers than we copied
    int32_t peerCount;       // real core count when truncated (0 otherwise)
    flclashtier_zt_peer peers[FLCLASHTIER_ZT_MAX_PEERS];
} flclashtier_zt_peer_list;

// Snapshot the peer list. Serialized on the same node mutex as every other
// ZT_Node_* entry point, so it can never race node_delete. Returns 1 when the
// query ran (count may be 0), 0 when there is no node or the core returned
// NULL. `out` is always zeroed first.
int flclashtier_zt_peers_export(flclashtier_zt_peer_list* out);

#ifdef __cplusplus
}
#endif

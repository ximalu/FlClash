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
#define FLCLASHTIER_ZT_FRAME_QUEUE 64

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

#ifdef __cplusplus
}
#endif

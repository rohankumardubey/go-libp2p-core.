package network

import (
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
)

// ResourceManager is the interface to the network resource management subsystem.
// The ResourceManager tracks and accounts for resource usage in the stack, from the internals
// to the application, and provides a mechanism to limit resource usage according to a user
// configurable policy.
//
// Resource Management through the ResourceManager is based on the concept of Resource
// Management Scopes, whereby resource usage is constrained by a DAG of scopes,
// The following diagram illustrates the structure of the resource constraint DAG:
// System
//   +------------> Transient.............+................+
//   |                                    .                .
//   +------------>  Service------------- . ----------+    .
//   |                                    .           |    .
//   +------------->  Protocol----------- . ----------+    .
//   |                                    .           |    .
//   +-------------->  Peer               \           |    .
//                      +------------> Connection     |    .
//                      |                             \    \
//                      +--------------------------->  Stream
//
// The basic resources accounted by the ResourceManager include memory, streams, connections,
// and file  descriptors. These account for both space and time used by
// the stack, as each resource has a direct effect on the system
// availability and performance.
//
// The modus operandi of the resource manager is to restrict resource usage at the time of
// reservation. When a component of the stack needs to use a resource, it reserves it in the
// appropriate scope. The resource manager gates the reservation against the scope applicable
// limits; if the limit is exceeded, then an error (wrapping ErrResourceLimitExceeded) and it
// is up the component to act accordingly. At the lower levels of the stack, this will normally
// signal a filure of some sorts, like failing to opening a stream or a connection, which will
// propagate to the programmer. Some components may be able to handle resource reservation failure
// more gracefully; for instance a muxer trying to grow a buffer for a window change, will simply
// retain the existing window size and continue to operate normally albeit with some degraded
// throughput.
// All resources reserved in some scope are released when the scope is closed. For low level
// scopes, mainly Connection and Stream scopes, this happens when the connection or stream is
// closed.
//
// Service programmers will typically use the resource manager to reserve memory
// for their subsystem.
// This happens with two avenues: the programmer can attach a stream to a service, whereby
// resources reserved by the stream are automatically accounted in the service budget; or the
// programmer may directly interact with the service scope, by using ViewService through the
// resource manager interface.
//
// Application programmers can also directly reserve memory in some applicable scope. In order
// to failicate control flow delimited resource accounting, all scopes defined in the system
// allow for the user to create transactions. Transactions are temporary scopes rooted at some
// other scope and release their resources when the programmer is done with them. Transaction
// scopes can form trees, with nested transactions.
//
// Typical Usage:
//  - Low level components of the system (transports, muxers) all have access to the resource
//    manageer and create connection and stream scopes through it. These scopes are accessible
//    to the user, albeit with a narrower interface, through Conn and Stream objects who have
//    a Scope method.
//  - Services typically center around streams, where the programmer can attach streams to a
//    particular service. They can also directly reserve memory for a service by accessing the
//    service scope using the ResourceManager interface.
//  - Applications that want to account for their network resource usage can reserve memory,
//    typically using a transaction, directly in the System or a Service scope; they can also
//    opt to use appropriate steam scopes for streams that they create or own.
//
// User Serviceable Parts: the user has the option to specify their own implementation of the
// interface. We provide a canonical implementation in the go-libp2p-resource-manager package.
// The user of that package can specify limits for the various scopes, which can be static
// or dynamic.
type ResourceManager interface {
	// ViewSystem views the system wide resource scope.
	// The system scope is the top level scope that accounts for global
	// resource usage at all levels of the system. This scope constrains all
	// other scopes and institutes global hard limits.
	ViewSystem(func(ResourceScope) error) error

	// ViewTransient views the transient (DMZ) resource scope.
	// The transient scope accounts for resources that are in the process of
	// full establishment.  For instance, a new connection prior to the
	// handshake does not belong to any peer, but it still needs to be
	// constrained as this opens an avenue for attacks in transient resource
	// usage. Similarly, a stream that has not negotiated a protocol yet is
	// constrained by the transient scope.
	ViewTransient(func(ResourceScope) error) error

	// ViewService retrieves a service-specific scope.
	ViewService(string, func(ServiceScope) error) error

	// ViewProtocol views the resource management scope for a specific protocol.
	ViewProtocol(protocol.ID, func(ProtocolScope) error) error

	// ViewPeer views the resource management scope for a specific peer.
	ViewPeer(peer.ID, func(PeerScope) error) error

	// OpenConnection creates a new connection scope not yet associated with any peer; the connection
	// is scoped at the transient scope.
	// The caller owns the returned scope and is responsible for calling Done in order to signify
	// the end of th scope's span.
	OpenConnection(dir Direction, usefd bool) (ConnManagementScope, error)

	// OpenStream creates a new stream scope, initially unnegotiated.
	// An unnegotiated stream will be initially unattached to any protocol scope
	// and constrained by the transient scope.
	// The caller owns the returned scope and is responsible for calling Done in order to signify
	// the end of th scope's span.
	OpenStream(p peer.ID, dir Direction) (StreamManagementScope, error)

	// Close closes the resource manager
	Close() error
}

// MemoryStatus is an indicator of the current level of available memory for scope reservations.
type MemoryStatus int

const (
	// MemoryStatusOK indicates that the scope has sufficient memory.
	MemoryStatusOK = iota
	// MemoryStatusCaution indicates that the scope is using more than half its available memory.
	MemoryStatusCaution
	// MemoryStatusCritical indicates that the scope is using more than 80% of its available memory.
	MemoryStatusCritical
)

// ResourceScope is the interface for all scopes.
type ResourceScope interface {
	// ReserveMemory reserves memory/buffer space in the scope.
	//
	// If ReserveMemory returns an error, then no memory was reserved and the caller should handle
	// the failure condition.
	//
	// If the error is nil, then the returned MemoryStatus indicates the health of the memory
	// subsystem for the scope, _after_ the reservation.
	// A MemoryStatus of MemoryStatusCritical (Red) indicates that the scope already uses
	// too much memory and it should only proceed with absolutely critical memory allocations.
	// A MemoryStatus of MemorStatusCaution (Yellow) indicates that the scope uses a lot
	// of memory and the caller should backoff if it is an optional operation (e.g. a window
	// buffer increase in a muxer).
	// A MemoryStatus of MemoryStatusOK (Green) indicates that the scope has sufficient memory
	// available and the caller is free to proceed without concerns.
	ReserveMemory(size int) (MemoryStatus, error)
	// ReleaseMemory explicitly releases memory previously reserved with ReserveMemory
	ReleaseMemory(size int)

	// Stat retrieves current resource usage for the scope.
	Stat() ScopeStat

	// BeginTransaction creates a new transactional scope rooted at this scope
	BeginTransaction() (TransactionalScope, error)
}

// TransactionalScope is a ResourceScope with transactional semantics.
// Transactional scopes are control flow delimited and release all their associated resources
// when the programmer calls Done.
//
// Example:
//  txn, err := someScope.BeginTransaction()
//  if err != nil { ... }
//  defer txn.Done()
//
//  if err := txn.ReserveMemory(...); err != nil { ... }
//  // ... use memory
type TransactionalScope interface {
	ResourceScope
	// Done ends the transaction scope and releases associated resources.
	Done()
}

// ServiceScope is the interface for service resource scopes
type ServiceScope interface {
	ResourceScope

	// Name returns the name of this service
	Name() string
}

// ProtocolScope is the interface for protocol resource scopes.
type ProtocolScope interface {
	ResourceScope

	// Protocol returns the protocol for this scope
	Protocol() protocol.ID
}

// PeerScope is the interface for peer resource scopes.
type PeerScope interface {
	ResourceScope

	// Peer returns the peer ID for this scope
	Peer() peer.ID
}

// ConnManagementScope is the low level interface for connection resource scopes.
// This interface is used by the low level components of the system who create and own
// the span of a connection scope.
type ConnManagementScope interface {
	TransactionalScope

	// PeerScope returns the peer scope associated with this connection.
	// It returns nil if the connection is not yet asociated with any peer.
	PeerScope() PeerScope

	// SetPeer sets the peer for a previously unassociated connection
	SetPeer(peer.ID) error
}

// ConnScope is the user view of a connection scope
type ConnScope interface {
	ResourceScope
}

// StreamManagementScope is the interface for stream resource scopes.
// This interface is used by the low level components of the system who create and own
// the span of a stream scope.
type StreamManagementScope interface {
	TransactionalScope

	// ProtocolScope returns the protocol resource scope associated with this stream.
	// It returns nil if the stream is not associated with any protocol scope.
	ProtocolScope() ProtocolScope
	// SetProtocol sets the protocol for a previously unnegotiated stream
	SetProtocol(proto protocol.ID) error

	// ServiceScope returns the service owning the stream, if any.
	ServiceScope() ServiceScope
	// SetService sets the service owning this stream.
	SetService(srv string) error

	// PeerScope returns the peer resource scope associated with this stream.
	PeerScope() PeerScope
}

// StreamScope is the user view of a StreamScope.
type StreamScope interface {
	ResourceScope

	// SetService sets the service owning this stream.
	SetService(srv string) error
}

// ScopeStat is a struct containing resource accounting information.
type ScopeStat struct {
	NumStreamsInbound  int
	NumStreamsOutbound int
	NumConnsInbound    int
	NumConnsOutbound   int
	NumFD              int

	Memory int64
}

// NullResourceManager is a stub for tests and initialization of default values
var NullResourceManager ResourceManager = &nullResourceManager{}

type nullResourceManager struct{}
type nullScope struct{}

var _ ResourceScope = (*nullScope)(nil)
var _ TransactionalScope = (*nullScope)(nil)
var _ ServiceScope = (*nullScope)(nil)
var _ ProtocolScope = (*nullScope)(nil)
var _ PeerScope = (*nullScope)(nil)
var _ ConnManagementScope = (*nullScope)(nil)
var _ ConnScope = (*nullScope)(nil)
var _ StreamManagementScope = (*nullScope)(nil)
var _ StreamScope = (*nullScope)(nil)

var nullScopeObj = &nullScope{}

func (n *nullResourceManager) ViewSystem(f func(ResourceScope) error) error {
	return f(nullScopeObj)
}
func (n *nullResourceManager) ViewTransient(f func(ResourceScope) error) error {
	return f(nullScopeObj)
}
func (n *nullResourceManager) ViewService(svc string, f func(ServiceScope) error) error {
	return f(nullScopeObj)
}
func (n *nullResourceManager) ViewProtocol(p protocol.ID, f func(ProtocolScope) error) error {
	return f(nullScopeObj)
}
func (n *nullResourceManager) ViewPeer(p peer.ID, f func(PeerScope) error) error {
	return f(nullScopeObj)
}
func (n *nullResourceManager) OpenConnection(dir Direction, usefd bool) (ConnManagementScope, error) {
	return nullScopeObj, nil
}
func (n *nullResourceManager) OpenStream(p peer.ID, dir Direction) (StreamManagementScope, error) {
	return nullScopeObj, nil
}
func (n *nullResourceManager) Close() error {
	return nil
}

func (n *nullScope) ReserveMemory(size int) (MemoryStatus, error)  { return MemoryStatusOK, nil }
func (n *nullScope) ReleaseMemory(size int)                        {}
func (n *nullScope) Stat() ScopeStat                               { return ScopeStat{} }
func (n *nullScope) BeginTransaction() (TransactionalScope, error) { return nullScopeObj, nil }
func (n *nullScope) Done()                                         {}
func (n *nullScope) Name() string                                  { return "" }
func (n *nullScope) Protocol() protocol.ID                         { return "" }
func (n *nullScope) Peer() peer.ID                                 { return "" }
func (n *nullScope) PeerScope() PeerScope                          { return nullScopeObj }
func (n *nullScope) SetPeer(peer.ID) error                         { return nil }
func (n *nullScope) ProtocolScope() ProtocolScope                  { return nullScopeObj }
func (n *nullScope) SetProtocol(proto protocol.ID) error           { return nil }
func (n *nullScope) ServiceScope() ServiceScope                    { return nullScopeObj }
func (n *nullScope) SetService(srv string) error                   { return nil }

package grpc

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// MethodInfo describes one RPC from a descriptor set.
type MethodInfo struct {
	ClientStreaming bool
	ServerStreaming bool
	// Desc carries the request and response message types (used for JSON
	// transcoding).
	Desc protoreflect.MethodDescriptor
}

// Kind is unary, client_streaming, server_streaming or bidi_streaming.
func (m MethodInfo) Kind() string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		return "bidi_streaming"
	case m.ClientStreaming:
		return "client_streaming"
	case m.ServerStreaming:
		return "server_streaming"
	}
	return "unary"
}

// ParseDescriptorSet reads a serialized google.protobuf.FileDescriptorSet
// (protoc --descriptor_set_out, with --include_imports) and returns its
// methods keyed by "package.Service/Method".
func ParseDescriptorSet(b []byte) (map[string]MethodInfo, error) {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("descriptor set is not a FileDescriptorSet: %w", err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("descriptor set is incomplete (build it with --include_imports): %w", err)
	}
	methods := map[string]MethodInfo{}
	for _, f := range set.File {
		fd, err := files.FindFileByPath(f.GetName())
		if err != nil {
			return nil, err
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				m := ms.Get(j)
				methods[string(svc.FullName())+"/"+string(m.Name())] = MethodInfo{
					ClientStreaming: m.IsStreamingClient(), ServerStreaming: m.IsStreamingServer(), Desc: m}
			}
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("descriptor set defines no services")
	}
	return methods, nil
}

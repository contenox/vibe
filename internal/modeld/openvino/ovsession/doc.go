// Package ovsession contains the native OpenVINO session/KV bridge used by the
// openvino modelrepo provider.
//
// The default build is a pure-Go stub. Build with -tags openvino and provide the
// OpenVINO C++ SDK include/lib flags to enable the CGo implementation.
package ovsession

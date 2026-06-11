// Package uapiref exposes ground-truth sizeof/offsetof/constant values from
// the real <linux/videodev2.h> kernel UAPI header via cgo. It exists solely so
// the layout verification test in the parent v4l2 package (layout_cgo_test.go)
// can compare the hand-written pure-Go struct definitions against the C
// compiler's view — Go forbids cgo directly inside _test.go files.
//
// Nothing outside that test should import this package: the production v4l2
// package must stay pure Go so the host builds with CGO_ENABLED=0 from any
// platform. The cgo content is guarded by `linux && cgo` build tags; this
// doc file keeps the package buildable (empty) everywhere else.
package uapiref

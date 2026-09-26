// csi-call makes the CreateVolume and NodeStageVolume calls hack/smoke.sh
// needs with values csc cannot express: csc splits every key=val list on
// commas, and cipher specs such as xchacha12,aes-adiantum-plain64 contain
// one. Each -p/-ctx/-secret flag here carries exactly one key=val.
//
//	csi-call -endpoint unix:///ctl.sock create -name vol -bytes N -p k=v ...
//	csi-call -endpoint unix:///node.sock stage -volume ID -staging DIR -ctx k=v -secret k=v ...
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// kv is a repeatable key=val flag split on the first '=' only.
type kv map[string]string

func (m kv) String() string { return "" }

func (m kv) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=val, got %q", s)
	}
	m[k] = v
	return nil
}

func main() {
	endpoint := flag.String("endpoint", "", "CSI endpoint, e.g. unix:///tmp/ctl.sock")
	flag.Parse()
	if *endpoint == "" || flag.NArg() < 1 {
		fail(fmt.Errorf("usage: csi-call -endpoint URL create|stage [flags]"))
	}
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch cmd, args := flag.Arg(0), flag.Args()[1:]; cmd {
	case "create":
		err = create(ctx, csi.NewControllerClient(conn), args)
	case "stage":
		err = stage(ctx, csi.NewNodeClient(conn), args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fail(err)
	}
}

func mountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

// create prints the volume ID on the first line, then one key=val line per
// volume-context entry, sorted.
func create(ctx context.Context, c csi.ControllerClient, args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	name := fs.String("name", "", "volume name")
	size := fs.Int64("bytes", 0, "required capacity in bytes")
	params := kv{}
	fs.Var(params, "p", "StorageClass parameter key=val (repeatable)")
	_ = fs.Parse(args)
	resp, err := c.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               *name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: *size},
		VolumeCapabilities: []*csi.VolumeCapability{mountCap()},
		Parameters:         params,
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.GetVolume().GetVolumeId())
	vc := resp.GetVolume().GetVolumeContext()
	keys := make([]string, 0, len(vc))
	for k := range vc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, vc[k])
	}
	return nil
}

func stage(ctx context.Context, c csi.NodeClient, args []string) error {
	fs := flag.NewFlagSet("stage", flag.ExitOnError)
	volume := fs.String("volume", "", "volume ID")
	staging := fs.String("staging", "", "staging target path")
	volCtx, secrets := kv{}, kv{}
	fs.Var(volCtx, "ctx", "volume context key=val (repeatable)")
	fs.Var(secrets, "secret", "node-stage secret key=val (repeatable)")
	_ = fs.Parse(args)
	_, err := c.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          *volume,
		StagingTargetPath: *staging,
		VolumeCapability:  mountCap(),
		VolumeContext:     volCtx,
		Secrets:           secrets,
	})
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "csi-call:", err)
	os.Exit(1)
}

# LUKS2 Encryption Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in per-StorageClass LUKS2 encryption of fileblock volumes, keyed by a node-stage Secret with `key`/`previousKey` rotation on next stage, plus moving `backingStore.nfs.subDir` to the provisioner's template token spelling.

**Architecture:** The node plugin layers dm-crypt between the loop device and ext4 (`loop → LUKS2 → ext4`). A new `pkg/crypt` is the only caller of `cryptsetup`; secrets reach it over inherited pipes via a new `exec.Runner.RunCmd`. The controller only truncates encrypted images; the node formats on first stage (blank-header check + LUKS2 label as the "needs mkfs" marker), rotates key slots, and opens the mapping. The state file and reconciler learn about crypt mappings.

**Tech Stack:** Go 1.25, CSI spec v1, cryptsetup 2.6 (`cryptsetup-bin`, Debian bookworm), util-linux losetup, e2fsprogs, kind for e2e.

**Spec:** `docs/superpowers/specs/2026-09-25-luks-encryption-design.md`

## Global Constraints

- StorageClass opt-in parameter: `encrypted`, values `"true"` / `"false"` / absent; anything else → `InvalidArgument`. Same key is used in volume context.
- Secret entries: `key` (required, ≥32 bytes, used byte-for-byte), `previousKey` (optional). No trimming, no base64 decoding.
- Secret values never appear in argv, logs, error strings, or on disk. Secrets reach child processes only through `exec.Cmd.Secrets` (`/dev/fd/3+i`).
- Every `cryptsetup` call runs with env `DM_DISABLE_UDEV=1`.
- `luksFormat` flags exactly: `--batch-mode --type luks2 --cipher aes-xts-plain64 --key-size 512 --pbkdf pbkdf2 --pbkdf-force-iterations 1000 --label fileblock-unformatted`.
- `luksAddKey` uses `--pbkdf pbkdf2 --pbkdf-force-iterations 1000`. `open` uses `--disable-keyring`.
- LUKS2 labels: `fileblock-unformatted` before mkfs, `fileblock` after.
- Mapper name: `"fbcrypt-" + first 24 hex chars of sha256(volumeID)`; path `/dev/mapper/<name>`.
- gRPC codes: missing `key` → `FailedPrecondition`; short `key` → `InvalidArgument`; no key opens → `PermissionDenied`; mapping already open or non-blank non-LUKS image → `FailedPrecondition`; other crypt failures → `Internal`.
- `store.Config` / `StoreID` are unchanged by encryption. No existing volumeID changes.
- subDir tokens become exactly `${pvc.namespace}`, `${pvc.name}`, `${pv.name}`; the old `${pvc.metadata.*}` spelling is NOT accepted as an alias.
- Unencrypted volumes' NodeStage command sequence stays byte-for-byte as today (attach → e2fsck → set-capacity → resize2fs → mount).
- Runtime image adds `cryptsetup-bin`. No RBAC change.
- Repo conventions (CLAUDE.md): every shell-out through `pkg/exec.Runner`; one short comment per non-obvious block; never panic in a handler; `make fmt-check vet lint tidy-check test build` must pass before every push.
- Environment note: this dev container has no root, no loop devices and no cryptsetup. `make smoke`, `make sanity` and `make e2e*` cannot run locally; they run in CI (`ci.yml` runs `make check`, including smoke and sanity, on every PR; `e2e.yml` runs on PRs too). Push and watch CI for those tasks.

## Review Focus

1. **A Secret created with `kubectl create secret --from-file`** carries a trailing newline — the key must be used verbatim (newline included) so the README recovery recipe (`… | base64 -d | cryptsetup open --key-file=-`) opens the same volume. Test: `TestKeysFromSecretsPreservesBytes` (Task 7).
2. **`previousKey` left in the Secret long after rotation** — every later stage must succeed, must not touch the header, and must never remove the slot `key` opens. Test: `TestPrepareStalePreviousIsIgnored` (Task 4).
3. **A tiny encrypted PVC (e.g. `10Mi`)** can't hold the 16 MiB LUKS2 header; the user expects an immediate, clear provisioning error, not a stage-time cryptsetup failure loop. Test: `TestCreateVolumeEncryptedTooSmallIsOutOfRange` (Task 6), minimum 32 MiB.
4. **A stage that fails after the mapping is open** (e.g. e2fsck fails) must close the mapping and detach the loop, or every kubelet retry hits "already open". Test: `TestNodeStageEncryptedFailureClosesMapping` (Task 7).
5. **A crashed plugin leaves an open `fbcrypt-*` mapping** — restart must close it before detaching its loop, or the loop can never be freed. Tests: `TestReconcileClosesOrphanCryptBeforeDetach` (Task 5) and the smoke orphan-mapping block (Task 8).

---

## File Structure

| File | Responsibility |
|---|---|
| `pkg/store/parse.go` (modify) | subDir token constants + comment |
| `pkg/exec/exec.go` (modify) | `Cmd`, `SecretFD`, `Runner.RunCmd`, pipe plumbing |
| `pkg/exec/exectest/fake.go` (modify) | `FakeRunner.RunCmd`, `CmdFunc`, `Call.Env/Secrets` |
| `pkg/image/resize.go` (modify) | `Mkfs` (shared mkfs.ext4 flags) |
| `pkg/image/image.go` (modify) | `CreateOptions{Unformatted}` |
| `pkg/crypt/crypt.go` (create) | `Crypt`, `Keys`, `Prepare`, `Close`, `Resize`, errors, `MapperName` |
| `pkg/crypt/sysfs.go` (create) | `List`, `IsOpen` from `/sys/block/dm-*` |
| `pkg/loop/state.go` (modify) | `Mapping.CryptDev` |
| `pkg/loop/reconcile.go` (modify) | crypt-aware reconcile |
| `pkg/driver/encryption.go` (create) | `ParamEncrypted`, `encryptedFromParams`, `keysFromSecrets`, `cryptStatus` |
| `pkg/driver/controller.go` (modify) | encrypted CreateVolume |
| `pkg/driver/node.go` (modify) | encrypted stage/unstage/expand |
| `cmd/node/main.go` (modify) | `/run/cryptsetup`, reconciler wiring |
| `Dockerfile`, `.github/workflows/ci.yml`, `hack/smoke.sh`, `hack/csi-sanity.sh` | packaging + integration |
| `hack/e2e.sh`, `test/e2e/encryption_test.go` | e2e |
| `README.md`, `CLAUDE.md`, `CHANGELOG.md`, `examples/storageclass-encrypted.yaml` | docs |

---

### Task 1: subDir moves to the provisioner's token spelling

**Files:**
- Modify: `pkg/store/parse.go:26-31` (constants), `:80-87` (resolveSubDir comment)
- Modify: `pkg/store/parse_test.go` (subDir token tests, ~lines 157-215)
- Modify: `pkg/driver/controller_test.go:378`
- Modify: `test/e2e/subdir_test.go:13,72,94`
- Modify: `deploy/manifests_test.go:146`, `deploy/kustomize/base/controller-deployment.yaml:65`
- Modify: `README.md` (subDir section ~147-160, config table ~205)

**Interfaces:**
- Consumes: nothing new.
- Produces: subDir accepts `${pvc.namespace}`, `${pvc.name}`, `${pv.name}`; anything else containing `${` is fatal.

- [ ] **Step 1: Rewrite the token tests in `pkg/store/parse_test.go`**

Replace `TestConfigFromParamsSubDirSubstitutesNamespace`, `TestConfigFromParamsSubDirSubstitutesPVCAndPVName`, `TestConfigFromParamsSubDirUnresolvedTokenIsFatal` and `TestConfigFromParamsSubDirMistypedTokenIsFatal` with:

```go
func TestConfigFromParamsSubDirSubstitutesNamespace(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":          "${pvc.namespace}/fileblock",
		"csi.storage.k8s.io/pvc/namespace": "team-a",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "team-a/fileblock" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "team-a/fileblock")
	}
}

func TestConfigFromParamsSubDirSubstitutesPVCAndPVName(t *testing.T) {
	c, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir":     "${pvc.name}/${pv.name}",
		"csi.storage.k8s.io/pvc/name": "my-claim",
		"csi.storage.k8s.io/pv/name":  "pv-123",
	}))
	if err != nil {
		t.Fatalf("ConfigFromParams: %v", err)
	}
	if c.NFSSubDir != "my-claim/pv-123" {
		t.Errorf("NFSSubDir = %q, want %q", c.NFSSubDir, "my-claim/pv-123")
	}
}

// Without --extra-create-metadata=true the metadata keys are absent, the
// token survives, and every namespace would land in one shared directory
// whose name looks like a template. That is the exact isolation failure
// the parameter exists to prevent, so it is fatal.
func TestConfigFromParamsSubDirUnresolvedTokenIsFatal(t *testing.T) {
	_, err := ConfigFromParams(nfsParams(map[string]string{
		"backingStore.nfs.subDir": "${pvc.namespace}/fileblock",
	}))
	if err == nil {
		t.Fatal("expected an error for an unresolved token")
	}
	for _, want := range []string{
		"backingStore.nfs.subDir",
		"${pvc.namespace}",
		"--extra-create-metadata=true",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The v0.4.0 spelling is gone, not aliased. It must fail loudly and the
// message must show the operator the spelling that replaced it.
func TestConfigFromParamsSubDirOldSpellingIsFatal(t *testing.T) {
	for _, old := range []string{"${pvc.metadata.namespace}", "${pvc.metadata.name}", "${pv.metadata.name}"} {
		_, err := ConfigFromParams(nfsParams(map[string]string{
			"backingStore.nfs.subDir":          old + "/fileblock",
			"csi.storage.k8s.io/pvc/namespace": "team-a",
			"csi.storage.k8s.io/pvc/name":      "my-claim",
			"csi.storage.k8s.io/pv/name":       "pv-123",
		}))
		if err == nil {
			t.Fatalf("%s: expected an error for the old spelling", old)
		}
		if !strings.Contains(err.Error(), old) {
			t.Errorf("%s: error %q should quote the unresolved token", old, err)
		}
		if !strings.Contains(err.Error(), "${pvc.namespace}") {
			t.Errorf("%s: error %q should list the supported tokens", old, err)
		}
	}
}
```

Also grep the file for any other `metadata.` token use and convert it to the new spelling.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./pkg/store/ -run SubDir -v`
Expected: FAIL — `${pvc.namespace}` is currently unresolved (Substitutes* tests fail), old-spelling test fails (old spelling resolves).

- [ ] **Step 3: Change the constants and comment in `pkg/store/parse.go`**

```go
	// Spelled as external-provisioner spells its node-stage-secret
	// templates, so every template in a fileblock StorageClass uses one
	// vocabulary. Those templates are the provisioner's and cannot change.
	tmplPVCName      = "${pvc.name}"
	tmplPVCNamespace = "${pvc.namespace}"
	tmplPVName       = "${pv.name}"
```

In the `resolveSubDir` doc comment, replace `"${pvc.metadata.namespace}"` with `"${pvc.namespace}"`.

- [ ] **Step 4: Update the other references**

- `pkg/driver/controller_test.go:378`: `params[store.ParamNFSSubDir] = "${pvc.namespace}/fileblock"`
- `test/e2e/subdir_test.go`: doc comment line 13 → `${pvc.namespace}`; line 72 `filepath.Join(export, "${pvc.namespace}")`; line 94 `backingStore.nfs.subDir: ${pvc.namespace}/fileblock`
- `deploy/manifests_test.go:146` comment → `${pvc.namespace}`
- `deploy/kustomize/base/controller-deployment.yaml:65` comment → `${pvc.namespace}`
- `README.md`: example line → `backingStore.nfs.subDir: ${pvc.namespace}/fileblock`; "Supported tokens are `${pvc.namespace}`, `${pvc.name}` and `${pv.name}` — the same spelling external-provisioner uses for `csi.storage.k8s.io/node-stage-secret-*`, so every template in a StorageClass reads alike." Remove the sentence saying the spelling matches csi-driver-nfs. Config table cell → `supports \`${pvc.namespace}\``.

Leave `docs/superpowers/specs/2026-09-21-*`, `docs/superpowers/plans/2026-09-22-*` and the `[0.4.0]` CHANGELOG section untouched.

- [ ] **Step 5: Run tests**

Run: `go test ./pkg/store/ ./pkg/driver/ ./deploy/ && go vet -tags=e2e ./test/e2e/ && grep -rn 'metadata\.namespace}\|metadata\.name}' --include=*.go --include=*.yaml --include=README.md . | grep -v docs/superpowers`
Expected: tests PASS; grep prints only `jsonpath={.items...metadata.name}` lines in `test/e2e` (kubectl jsonpath, unrelated).

- [ ] **Step 6: Commit**

```bash
git add pkg/store pkg/driver/controller_test.go test/e2e/subdir_test.go deploy README.md
git commit -m "store: spell subDir tokens as external-provisioner does

\${pvc.namespace}, \${pvc.name} and \${pv.name} replace the
\${pvc.metadata.*} spelling. Breaking for StorageClasses using the old
tokens: their next CreateVolume fails with InvalidArgument."
```

---

### Task 2: `exec.Runner.RunCmd` — env and secrets over inherited pipes

**Files:**
- Modify: `pkg/exec/exec.go`
- Modify: `pkg/exec/exec_test.go`
- Modify: `pkg/exec/exectest/fake.go`

**Interfaces:**
- Produces:
  ```go
  type Cmd struct { Name string; Args []string; Env []string; Secrets [][]byte }
  func SecretFD(i int) string // "/dev/fd/3" for i == 0
  type Runner interface {
      Run(ctx context.Context, name string, args ...string) (string, error)
      RunCmd(ctx context.Context, c Cmd) (string, error)
  }
  // exectest:
  type Call struct { Name string; Args []string; Env []string; Secrets [][]byte }
  (*FakeRunner).CmdFunc func(ctx context.Context, c fbexec.Cmd) (string, error)
  (*FakeRunner).RunCmd(ctx context.Context, c fbexec.Cmd) (string, error)
  ```

- [ ] **Step 1: Write failing tests in `pkg/exec/exec_test.go`**

```go
func TestRunCmdDeliversSecretsOnFDs(t *testing.T) {
	r := New(0)
	out, err := r.RunCmd(context.Background(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", `cat "$0"; printf '|'; cat "$1"`, SecretFD(0), SecretFD(1)},
		Secrets: [][]byte{[]byte("alpha\n"), []byte("beta")},
	})
	if err != nil {
		t.Fatalf("RunCmd: %v", err)
	}
	if out != "alpha\n|beta" {
		t.Fatalf("out = %q, want %q", out, "alpha\n|beta")
	}
}

func TestRunCmdAppendsEnv(t *testing.T) {
	out, err := New(0).RunCmd(context.Background(), Cmd{
		Name: "sh",
		Args: []string{"-c", `printf %s "$FB_TEST_ENV"`},
		Env:  []string{"FB_TEST_ENV=set"},
	})
	if err != nil || out != "set" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunCmdErrorOmitsSecret(t *testing.T) {
	_, err := New(0).RunCmd(context.Background(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", `cat "$0" >/dev/null; exit 3`, SecretFD(0)},
		Secrets: [][]byte{[]byte("s3cr3t-value")},
	})
	var e *Error
	if !errors.As(err, &e) || e.ExitCode != 3 {
		t.Fatalf("err = %v, want *Error exit 3", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

func TestSecretFD(t *testing.T) {
	if SecretFD(0) != "/dev/fd/3" || SecretFD(2) != "/dev/fd/5" {
		t.Fatalf("SecretFD: %s %s", SecretFD(0), SecretFD(2))
	}
}
```

Add `errors` and `strings` imports if missing.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./pkg/exec/ -run 'RunCmd|SecretFD' -v`
Expected: FAIL to compile — `Cmd`, `RunCmd`, `SecretFD` undefined.

- [ ] **Step 3: Implement in `pkg/exec/exec.go`**

Add imports `os`, `strconv`, `sync`. Replace the `Runner` interface and `osRunner.Run`:

```go
// Cmd is a command with inputs Run cannot express.
type Cmd struct {
	Name string
	Args []string
	// Env is appended to the parent's environment.
	Env []string
	// Secrets[i] is readable by the child, to EOF, at SecretFD(i). They
	// travel over pipes so they never touch argv or disk.
	Secrets [][]byte
}

// SecretFD is the path at which the child reads Cmd.Secrets[i].
func SecretFD(i int) string { return "/dev/fd/" + strconv.Itoa(3+i) }

// Runner is the interface the rest of the driver depends on. Tests substitute
// a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunCmd(ctx context.Context, c Cmd) (string, error)
}

func (r *osRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.RunCmd(ctx, Cmd{Name: name, Args: args})
}

func (r *osRunner) RunCmd(ctx context.Context, c Cmd) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	writers := make([]*os.File, 0, len(c.Secrets))
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			_ = f.Close()
		}
	}
	for range c.Secrets {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeAll(cmd.ExtraFiles)
			closeAll(writers)
			return "", fmt.Errorf("pipe for %s: %w", c.Name, err)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, pr)
		writers = append(writers, pw)
	}
	startErr := cmd.Start()
	// The child holds its own copies of the read ends.
	closeAll(cmd.ExtraFiles)
	if startErr != nil {
		closeAll(writers)
		return "", &Error{Cmd: c.Name, Args: c.Args, ExitCode: -1, Err: startErr}
	}
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(w *os.File, s []byte) {
			defer wg.Done()
			_, _ = w.Write(s)
			_ = w.Close()
		}(w, c.Secrets[i])
	}
	err := cmd.Wait()
	wg.Wait()
	out := buf.String()
	if err != nil {
		return out, &Error{
			Cmd:      c.Name,
			Args:     c.Args,
			ExitCode: cmd.ProcessState.ExitCode(),
			Output:   out,
			Err:      err,
		}
	}
	return out, nil
}
```

(A child that exits without reading gets its pipe closed; the writer goroutine then fails with EPIPE and exits, so `wg.Wait` cannot hang.)

- [ ] **Step 4: Extend `pkg/exec/exectest/fake.go`**

Import `fbexec "github.com/middlendian/fileblock-csi/pkg/exec"`. Change `Call`, add `CmdFunc`, route both entry points through one dispatcher:

```go
// Call records one Run or RunCmd invocation.
type Call struct {
	Name    string
	Args    []string
	Env     []string
	Secrets [][]byte
}
```

Add field to `FakeRunner`:

```go
	// CmdFunc, when set, handles RunCmd calls and sees Env and Secrets.
	// RunCmd falls back to Func and the rules when it is nil.
	CmdFunc func(ctx context.Context, c fbexec.Cmd) (string, error)
```

Replace `Run` with:

```go
// Run implements exec.Runner.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f.dispatch(ctx, fbexec.Cmd{Name: name, Args: args}, false)
}

// RunCmd implements exec.Runner.
func (f *FakeRunner) RunCmd(ctx context.Context, c fbexec.Cmd) (string, error) {
	return f.dispatch(ctx, c, true)
}

func (f *FakeRunner) dispatch(ctx context.Context, c fbexec.Cmd, viaCmd bool) (string, error) {
	f.mu.Lock()
	call := Call{Name: c.Name, Args: append([]string(nil), c.Args...), Env: append([]string(nil), c.Env...)}
	for _, s := range c.Secrets {
		call.Secrets = append(call.Secrets, append([]byte(nil), s...))
	}
	f.Calls = append(f.Calls, call)
	rule, ok := f.rules[c.Name]
	useDefault := f.HasDef
	def := f.Default
	fn := f.Func
	cmdFn := f.CmdFunc
	f.mu.Unlock()

	if viaCmd && cmdFn != nil {
		return cmdFn(ctx, c)
	}
	if fn != nil {
		return fn(ctx, c.Name, c.Args...)
	}
	if ok {
		return rule.Out, rule.Err
	}
	if useDefault {
		return def.Out, def.Err
	}
	return "", fmt.Errorf("FakeRunner: unexpected call %s %v", c.Name, c.Args)
}
```

- [ ] **Step 5: Run the whole suite (interface change touches every package)**

Run: `go build ./... && go test ./...`
Expected: PASS. If any other type implements `Runner` (none today: `grep -rn "func (.*) Run(ctx context.Context" --include=*.go .`), add `RunCmd` to it.

- [ ] **Step 6: Commit**

```bash
git add pkg/exec
git commit -m "exec: RunCmd passes env and secrets over inherited pipes"
```

---

### Task 3: `image.Mkfs` and unformatted `Create`

**Files:**
- Modify: `pkg/image/resize.go` (add `Mkfs`)
- Modify: `pkg/image/image.go` (`CreateOptions`, `Manager.Create` signature)
- Modify: `pkg/image/image_test.go`
- Modify: `pkg/driver/controller.go:98`, `pkg/driver/controller_test.go` (`fakeImages.Create`)

**Interfaces:**
- Produces:
  ```go
  func Mkfs(ctx context.Context, r fbexec.Runner, target string) error
  type CreateOptions struct{ Unformatted bool }
  Create(ctx context.Context, volumeID string, capacityBytes int64, opts CreateOptions) (*Metadata, error)
  ```

- [ ] **Step 1: Write failing tests in `pkg/image/image_test.go`**

```go
func TestMkfsArgs(t *testing.T) {
	fake := exectest.New()
	fake.SetDefault("", nil)
	if err := Mkfs(context.Background(), fake, "/dev/mapper/fbcrypt-x"); err != nil {
		t.Fatalf("Mkfs: %v", err)
	}
	want := []string{"-q", "-F", "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1", "/dev/mapper/fbcrypt-x"}
	if len(fake.Calls) != 1 || fake.Calls[0].Name != "mkfs.ext4" || !slices.Equal(fake.Calls[0].Args, want) {
		t.Fatalf("calls = %+v", fake.Calls)
	}
}

// Encrypted volumes are formatted by the node inside the LUKS mapping, so
// the controller must leave the sparse file exactly as truncated: all
// zeros, which is what the node's blank-header check relies on.
func TestCreateUnformattedSkipsMkfs(t *testing.T) {
	fake := exectest.New() // any call fails the test via "unexpected call"
	mgr, err := New(t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := mgr.Create(context.Background(), "fb-enc", 32<<20, CreateOptions{Unformatted: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.CapacityBytes != 32<<20 {
		t.Fatalf("capacity %d", meta.CapacityBytes)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("unexpected shell-outs: %+v", fake.Calls)
	}
	st, err := os.Stat(mgr.ImagePath("fb-enc"))
	if err != nil || st.Size() != 32<<20 {
		t.Fatalf("stat: %v size=%v", err, st)
	}
}
```

Imports: `slices`, `github.com/middlendian/fileblock-csi/pkg/exec/exectest`. Update every existing `mgr.Create(ctx, id, n)` in the file to `mgr.Create(ctx, id, n, CreateOptions{})`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./pkg/image/ -v`
Expected: FAIL to compile (`Mkfs`, `CreateOptions` undefined).

- [ ] **Step 3: Implement**

`pkg/image/resize.go`:

```go
// Mkfs makes the ext4 filesystem every fileblock volume uses on target,
// which is either an .img file or a block device.
func Mkfs(ctx context.Context, r fbexec.Runner, target string) error {
	if _, err := r.Run(ctx, "mkfs.ext4", "-q", "-F",
		"-m", "0",
		"-E", "lazy_itable_init=1,lazy_journal_init=1",
		target); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w", target, err)
	}
	return nil
}
```

`pkg/image/image.go`:

```go
// CreateOptions tunes Create.
type CreateOptions struct {
	// Unformatted leaves the sparse file without a filesystem. Encrypted
	// volumes use it: the node formats inside the LUKS mapping.
	Unformatted bool
}
```

Interface line: `Create(ctx context.Context, volumeID string, capacityBytes int64, opts CreateOptions) (*Metadata, error)`. In `fsManager.Create` change the signature and replace the mkfs block with:

```go
	if err := truncateSparse(imgPath, capacityBytes); err != nil {
		return nil, err
	}
	if opts.Unformatted {
		return &Metadata{VolumeID: volumeID, CapacityBytes: capacityBytes}, nil
	}
	if err := Mkfs(ctx, m.exec, imgPath); err != nil {
		_ = os.Remove(imgPath)
		return nil, err
	}
```

`pkg/driver/controller.go:98`: `meta, err := images.Create(ctx, volumeID, int64(capacity), image.CreateOptions{})` (Task 6 sets the flag).

`pkg/driver/controller_test.go` `fakeImages`: add field `lastCreateOpts image.CreateOptions`; signature `Create(_ context.Context, volumeID string, capacityBytes int64, opts image.CreateOptions)`; first line inside the lock `f.lastCreateOpts = opts`.

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./pkg/image/ ./pkg/driver/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/image pkg/driver/controller.go pkg/driver/controller_test.go
git commit -m "image: extract Mkfs; Create can leave the image unformatted"
```

---

### Task 4: `pkg/crypt`

**Files:**
- Create: `pkg/crypt/crypt.go`, `pkg/crypt/sysfs.go`
- Test: `pkg/crypt/crypt_test.go`, `pkg/crypt/sysfs_test.go`

**Interfaces:**
- Consumes: `fbexec.Cmd`, `fbexec.SecretFD`, `fbexec.Error` (Task 2); `image.Mkfs` (Task 3).
- Produces:
  ```go
  const MinKeyLen = 32
  var ErrWrongKey, ErrNotBlank error
  type Keys struct{ Current, Previous []byte }
  type Mapping struct{ Name, Backing string } // Backing is "/dev/loopN" or ""
  func New(r fbexec.Runner) *Crypt            // sysfs root "/sys"
  func NewAt(r fbexec.Runner, sysRoot string) *Crypt
  func MapperName(volumeID string) string
  func MapperPath(name string) string
  func (c *Crypt) Prepare(ctx context.Context, dev, name string, k Keys) (string, error)
  func (c *Crypt) Close(ctx context.Context, name string) error
  func (c *Crypt) Resize(ctx context.Context, name string) error
  func (c *Crypt) List(ctx context.Context) ([]Mapping, error)
  func (c *Crypt) IsOpen(ctx context.Context, name string) (bool, error)
  ```

- [ ] **Step 1: Write `pkg/crypt/crypt_test.go` with a behavioral fake LUKS device**

```go
package crypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

var (
	keyA = []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	keyB = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	keyC = []byte("cccccccccccccccccccccccccccccccccccccccc")
)

// fakeLUKS models the header state cryptsetup would mutate, so tests
// assert outcomes (which slots exist, the label) rather than scripts.
type fakeLUKS struct {
	t      *testing.T
	luks   bool
	label  string
	slots  [][]byte
	opened bool
	subs   []string // cryptsetup subcommands (and "mkfs") in call order
}

func exitErr(code int) error { return &fbexec.Error{Cmd: "cryptsetup", ExitCode: code} }

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func (f *fakeLUKS) has(k []byte) bool {
	return slices.ContainsFunc(f.slots, func(s []byte) bool { return bytes.Equal(s, k) })
}

func (f *fakeLUKS) run(_ context.Context, c fbexec.Cmd) (string, error) {
	for _, s := range c.Secrets {
		for _, a := range c.Args {
			if strings.Contains(a, string(s)) {
				f.t.Fatalf("secret leaked into argv: %v", c.Args)
			}
		}
	}
	if c.Name == "mkfs.ext4" {
		f.subs = append(f.subs, "mkfs")
		return "", nil
	}
	if c.Name != "cryptsetup" {
		return "", fmt.Errorf("unexpected command %s", c.Name)
	}
	if !slices.Contains(c.Env, "DM_DISABLE_UDEV=1") {
		f.t.Fatalf("cryptsetup %v without DM_DISABLE_UDEV=1", c.Args)
	}
	sub := c.Args[0]
	f.subs = append(f.subs, sub)
	switch sub {
	case "isLuks":
		if f.luks {
			return "", nil
		}
		return "", exitErr(1)
	case "luksFormat":
		f.luks, f.label, f.slots = true, argAfter(c.Args, "--label"), [][]byte{c.Secrets[0]}
	case "open":
		if !f.has(c.Secrets[0]) {
			return "", exitErr(2)
		}
		if !slices.Contains(c.Args, "--test-passphrase") {
			f.opened = true
		}
	case "luksAddKey":
		if !f.has(c.Secrets[0]) {
			return "", exitErr(2)
		}
		f.slots = append(f.slots, c.Secrets[1])
	case "luksRemoveKey":
		i := slices.IndexFunc(f.slots, func(s []byte) bool { return bytes.Equal(s, c.Secrets[0]) })
		if i < 0 {
			return "", exitErr(2)
		}
		f.slots = slices.Delete(f.slots, i, i+1)
	case "luksDump":
		return "LUKS header information\nVersion:       \t2\nLabel:          " + f.label + "\nSubsystem:      (no subsystem)\n", nil
	case "config":
		f.label = argAfter(c.Args, "--label")
	case "close":
		if !f.opened {
			return "", exitErr(4)
		}
		f.opened = false
	case "resize":
	default:
		return "", fmt.Errorf("unexpected cryptsetup %s", sub)
	}
	return "", nil
}

func newFake(t *testing.T, f *fakeLUKS) *Crypt {
	f.t = t
	r := exectest.New()
	r.CmdFunc = f.run
	r.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		return f.run(ctx, fbexec.Cmd{Name: name, Args: args})
	}
	return NewAt(r, t.TempDir())
}

// blankDev is a sparse all-zero file standing in for a fresh loop device.
func blankDev(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dev")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrepareFormatsBlankDevice(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	path, err := c.Prepare(context.Background(), blankDev(t), "fbcrypt-x", Keys{Current: keyA})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if path != "/dev/mapper/fbcrypt-x" {
		t.Fatalf("path = %q", path)
	}
	if !f.opened || f.label != "fileblock" || len(f.slots) != 1 || !f.has(keyA) {
		t.Fatalf("state after first stage: %+v", f)
	}
	if !slices.Contains(f.subs, "luksFormat") || !slices.Contains(f.subs, "mkfs") {
		t.Fatalf("subs = %v", f.subs)
	}
}

func TestPrepareRefusesNonBlankNonLUKS(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	dev := blankDev(t)
	fh, _ := os.OpenFile(dev, os.O_WRONLY, 0)
	_, _ = fh.WriteAt([]byte{0x53}, 1080) // where an ext4 magic would sit
	_ = fh.Close()
	_, err := c.Prepare(context.Background(), dev, "fbcrypt-x", Keys{Current: keyA})
	if !errors.Is(err, ErrNotBlank) {
		t.Fatalf("err = %v, want ErrNotBlank", err)
	}
	if slices.Contains(f.subs, "luksFormat") {
		t.Fatal("formatted a non-blank device")
	}
}

// A crash between luksFormat and mkfs leaves the unformatted label; the
// next stage must finish the job.
func TestPrepareUnformattedLabelRunsMkfs(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock-unformatted", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !slices.Contains(f.subs, "mkfs") || f.label != "fileblock" {
		t.Fatalf("subs=%v label=%q", f.subs, f.label)
	}
}

func TestPrepareFormattedSkipsMkfs(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if slices.Contains(f.subs, "mkfs") || slices.Contains(f.subs, "luksFormat") {
		t.Fatalf("subs = %v", f.subs)
	}
}

func TestPrepareRotatesPreviousToCurrent(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) {
		t.Fatalf("slots after rotation: %q", f.slots)
	}
	add, rm := slices.Index(f.subs, "luksAddKey"), slices.Index(f.subs, "luksRemoveKey")
	if add < 0 || rm < 0 || add > rm {
		t.Fatalf("want luksAddKey before luksRemoveKey, subs = %v", f.subs)
	}
}

func TestPrepareFinishesInterruptedRotation(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA, keyB}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) || slices.Contains(f.subs, "luksAddKey") {
		t.Fatalf("slots=%q subs=%v", f.slots, f.subs)
	}
}

// Review Focus 2: previousKey left in the Secret after rotation finished.
func TestPrepareStalePreviousIsIgnored(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyB}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyB, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || !f.has(keyB) {
		t.Fatalf("slots = %q", f.slots)
	}
	if slices.Contains(f.subs, "luksAddKey") || slices.Contains(f.subs, "luksRemoveKey") {
		t.Fatalf("header touched: %v", f.subs)
	}
}

func TestPrepareWrongKey(t *testing.T) {
	for _, k := range []Keys{{Current: keyB, Previous: keyA}, {Current: keyB}} {
		f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyC}}
		c := newFake(t, f)
		_, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", k)
		if !errors.Is(err, ErrWrongKey) {
			t.Fatalf("err = %v, want ErrWrongKey", err)
		}
		if f.opened || len(f.slots) != 1 || !f.has(keyC) {
			t.Fatalf("state changed on wrong key: %+v", f)
		}
	}
}

func TestPreparePreviousEqualsCurrent(t *testing.T) {
	f := &fakeLUKS{luks: true, label: "fileblock", slots: [][]byte{keyA}}
	c := newFake(t, f)
	if _, err := c.Prepare(context.Background(), "/dev/loop9", "fbcrypt-x", Keys{Current: keyA, Previous: keyA}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.slots) != 1 || slices.Contains(f.subs, "luksRemoveKey") {
		t.Fatalf("slots=%q subs=%v", f.slots, f.subs)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := &fakeLUKS{}
	c := newFake(t, f)
	if err := c.Close(context.Background(), "fbcrypt-x"); err != nil {
		t.Fatalf("Close of a closed mapping: %v", err)
	}
}

func TestMapperName(t *testing.T) {
	long := strings.Repeat("v", 300)
	a, b := MapperName("fb-abc-vol1"), MapperName("fb-abc-vol2")
	if a == b || a != MapperName("fb-abc-vol1") {
		t.Fatalf("not deterministic/unique: %s %s", a, b)
	}
	if !strings.HasPrefix(a, "fbcrypt-") || len(a) != len("fbcrypt-")+24 || len(MapperName(long)) > 127 {
		t.Fatalf("bad name %q", a)
	}
	if MapperPath(a) != "/dev/mapper/"+a {
		t.Fatalf("MapperPath = %q", MapperPath(a))
	}
}
```

- [ ] **Step 2: Write `pkg/crypt/sysfs_test.go`**

```go
package crypt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mkDM(t *testing.T, root, dm, name, slave string) {
	t.Helper()
	d := filepath.Join(root, "block", dm)
	if err := os.MkdirAll(filepath.Join(d, "dm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "dm", "name"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if slave != "" {
		if err := os.MkdirAll(filepath.Join(d, "slaves", slave), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAndIsOpen(t *testing.T) {
	root := t.TempDir()
	mkDM(t, root, "dm-0", "fbcrypt-abc", "loop3")
	mkDM(t, root, "dm-1", "ubuntu--vg-root", "sda3")
	c := NewAt(nil, root)
	ms, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Name != "fbcrypt-abc" || ms[0].Backing != "/dev/loop3" {
		t.Fatalf("List = %+v", ms)
	}
	if open, _ := c.IsOpen(context.Background(), "fbcrypt-abc"); !open {
		t.Fatal("IsOpen(fbcrypt-abc) = false")
	}
	if open, _ := c.IsOpen(context.Background(), "fbcrypt-zzz"); open {
		t.Fatal("IsOpen(fbcrypt-zzz) = true")
	}
}

func TestListEmptySysfs(t *testing.T) {
	ms, err := NewAt(nil, t.TempDir()).List(context.Background())
	if err != nil || len(ms) != 0 {
		t.Fatalf("List = %v, %v", ms, err)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./pkg/crypt/ -v`
Expected: FAIL to compile (package has no non-test files / symbols undefined).

- [ ] **Step 4: Implement `pkg/crypt/crypt.go`**

```go
// Package crypt layers LUKS2 (dm-crypt) between a volume's loop device and
// its ext4 filesystem. It is the only package that runs cryptsetup.
package crypt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/image"
)

// MinKeyLen is the shortest accepted passphrase. It is what makes the
// cheap PBKDF below safe: 32 random bytes need no key stretching.
const MinKeyLen = 32

const (
	mapperPrefix     = "fbcrypt-"
	labelUnformatted = "fileblock-unformatted"
	labelFormatted   = "fileblock"
	blankCheckBytes  = 16 << 20 // the LUKS2 header area
	pbkdfIterations  = "1000"
)

var (
	// ErrWrongKey means neither supplied key opens the volume.
	ErrWrongKey = errors.New("no supplied key opens the volume")
	// ErrNotBlank means the device is neither LUKS nor a fresh image.
	ErrNotBlank = errors.New("not a LUKS volume and not blank; refusing to format")
)

// Keys are the passphrases from a volume's node-stage secret.
type Keys struct{ Current, Previous []byte }

// Crypt runs cryptsetup and reads dm state from sysfs.
type Crypt struct {
	exec    fbexec.Runner
	sysRoot string
}

func New(r fbexec.Runner) *Crypt { return NewAt(r, "/sys") }

// NewAt reads dm state under sysRoot instead of /sys; tests use it.
func NewAt(r fbexec.Runner, sysRoot string) *Crypt { return &Crypt{exec: r, sysRoot: sysRoot} }

// MapperName is deterministic and length-bounded: dm names cap at 127
// bytes and volumeIDs don't.
func MapperName(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return mapperPrefix + hex.EncodeToString(sum[:])[:24]
}

func MapperPath(name string) string { return "/dev/mapper/" + name }

// Without DM_DISABLE_UDEV libdevmapper waits on a udev cookie that never
// arrives: the node container has no udevd.
func (c *Crypt) cryptsetup(ctx context.Context, secrets [][]byte, args ...string) (string, error) {
	return c.exec.RunCmd(ctx, fbexec.Cmd{
		Name:    "cryptsetup",
		Args:    args,
		Env:     []string{"DM_DISABLE_UDEV=1"},
		Secrets: secrets,
	})
}

func exitCode(err error) int {
	var e *fbexec.Error
	if errors.As(err, &e) {
		return e.ExitCode
	}
	return -1
}

// Prepare formats dev on first use, moves its key slot to k.Current, opens
// it as name, and makes the filesystem if it has none yet. It returns the
// mapper path. Every step converges if a previous attempt crashed midway.
func (c *Crypt) Prepare(ctx context.Context, dev, name string, k Keys) (string, error) {
	luks, err := c.isLuks(ctx, dev)
	if err != nil {
		return "", err
	}
	if !luks {
		// Only a freshly truncated image is formatted; a header we can't
		// read is never overwritten.
		blank, err := isBlank(dev)
		if err != nil {
			return "", err
		}
		if !blank {
			return "", fmt.Errorf("%s: %w", dev, ErrNotBlank)
		}
		if _, err := c.cryptsetup(ctx, [][]byte{k.Current}, "luksFormat", "--batch-mode",
			"--type", "luks2", "--cipher", "aes-xts-plain64", "--key-size", "512",
			"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", pbkdfIterations,
			"--label", labelUnformatted, "--key-file", fbexec.SecretFD(0), dev); err != nil {
			return "", fmt.Errorf("luksFormat %s: %w", dev, err)
		}
	}
	if err := c.rotate(ctx, dev, k); err != nil {
		return "", err
	}
	if _, err := c.cryptsetup(ctx, [][]byte{k.Current}, "open", "--type", "luks2",
		"--disable-keyring", "--key-file", fbexec.SecretFD(0), dev, name); err != nil {
		return "", fmt.Errorf("open %s: %w", dev, err)
	}
	path := MapperPath(name)
	if err := c.ensureFilesystem(ctx, dev, path); err != nil {
		_ = c.Close(ctx, name)
		return "", err
	}
	return path, nil
}

// The label, not blkid, says whether mkfs is still owed: a real ext4 with
// a damaged superblock also shows no signature.
func (c *Crypt) ensureFilesystem(ctx context.Context, dev, path string) error {
	out, err := c.cryptsetup(ctx, nil, "luksDump", dev)
	if err != nil {
		return fmt.Errorf("luksDump %s: %w", dev, err)
	}
	if luksLabel(out) != labelUnformatted {
		return nil
	}
	if err := image.Mkfs(ctx, c.exec, path); err != nil {
		return err
	}
	if _, err := c.cryptsetup(ctx, nil, "config", "--label", labelFormatted, dev); err != nil {
		return fmt.Errorf("relabel %s: %w", dev, err)
	}
	return nil
}

func luksLabel(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		if v, ok := strings.CutPrefix(line, "Label:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// rotate leaves exactly the slot k.Current opens. Add-then-remove, so a
// crash between the two leaves both slots and the next call removes the
// old one.
func (c *Crypt) rotate(ctx context.Context, dev string, k Keys) error {
	cur, err := c.opens(ctx, dev, k.Current)
	if err != nil {
		return err
	}
	if len(k.Previous) == 0 || bytes.Equal(k.Previous, k.Current) {
		if !cur {
			return fmt.Errorf("%s: %w", dev, ErrWrongKey)
		}
		return nil
	}
	prev, err := c.opens(ctx, dev, k.Previous)
	if err != nil {
		return err
	}
	if !cur && !prev {
		return fmt.Errorf("%s: %w", dev, ErrWrongKey)
	}
	if !prev {
		return nil
	}
	if !cur {
		if _, err := c.cryptsetup(ctx, [][]byte{k.Previous, k.Current}, "luksAddKey", "--batch-mode",
			"--pbkdf", "pbkdf2", "--pbkdf-force-iterations", pbkdfIterations,
			"--key-file", fbexec.SecretFD(0), dev, fbexec.SecretFD(1)); err != nil {
			return fmt.Errorf("luksAddKey %s: %w", dev, err)
		}
	}
	if _, err := c.cryptsetup(ctx, [][]byte{k.Previous}, "luksRemoveKey", "--batch-mode",
		"--key-file", fbexec.SecretFD(0), dev); err != nil {
		return fmt.Errorf("luksRemoveKey %s: %w", dev, err)
	}
	return nil
}

// opens reports whether key unlocks a slot. cryptsetup exits 2 (EPERM)
// for a wrong passphrase.
func (c *Crypt) opens(ctx context.Context, dev string, key []byte) (bool, error) {
	_, err := c.cryptsetup(ctx, [][]byte{key}, "open", "--test-passphrase", "--type", "luks2",
		"--key-file", fbexec.SecretFD(0), dev)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 2:
		return false, nil
	default:
		return false, fmt.Errorf("test passphrase on %s: %w", dev, err)
	}
}

// isLuks: cryptsetup exits 1 (EINVAL) for a device with no LUKS header.
func (c *Crypt) isLuks(ctx context.Context, dev string) (bool, error) {
	_, err := c.cryptsetup(ctx, nil, "isLuks", dev)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	default:
		return false, fmt.Errorf("isLuks %s: %w", dev, err)
	}
}

func isBlank(dev string) (bool, error) {
	f, err := os.Open(dev) // #nosec G304 -- dev is the loop device this stage attached
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for read := 0; read < blankCheckBytes; {
		n, err := f.Read(buf[:min(len(buf), blankCheckBytes-read)])
		if bytes.ContainsFunc(buf[:n], func(r rune) bool { return r != 0 }) {
			return false, nil
		}
		read += n
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read %s: %w", dev, err)
		}
	}
	return true, nil
}

// Close is idempotent: cryptsetup exits 4 (ENODEV) for an inactive name.
func (c *Crypt) Close(ctx context.Context, name string) error {
	_, err := c.cryptsetup(ctx, nil, "close", name)
	if err == nil || exitCode(err) == 4 {
		return nil
	}
	return fmt.Errorf("close %s: %w", name, err)
}

// Resize grows an open mapping to its backing device. It needs no key
// because Prepare opens with --disable-keyring.
func (c *Crypt) Resize(ctx context.Context, name string) error {
	if _, err := c.cryptsetup(ctx, nil, "resize", name); err != nil {
		return fmt.Errorf("resize %s: %w", name, err)
	}
	return nil
}
```

Note on `bytes.ContainsFunc`: it decodes runes; a zero byte decodes to rune 0 and any non-zero byte to a non-zero rune (invalid UTF-8 → `utf8.RuneError`, non-zero), so the check is exact.

- [ ] **Step 5: Implement `pkg/crypt/sysfs.go`**

```go
package crypt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Mapping is an open fileblock dm-crypt device.
type Mapping struct {
	Name    string
	Backing string // "/dev/loopN", or "" if sysfs lists no slave
}

// List returns open fbcrypt-* mappings, read from sysfs so it needs no
// dmsetup and no udev.
func (c *Crypt) List(_ context.Context) ([]Mapping, error) {
	dms, err := filepath.Glob(filepath.Join(c.sysRoot, "block", "dm-*"))
	if err != nil {
		return nil, err
	}
	var out []Mapping
	for _, d := range dms {
		b, err := os.ReadFile(filepath.Join(d, "dm", "name")) // #nosec G304 -- sysfs path from Glob
		if err != nil {
			continue // removed while we scanned
		}
		name := strings.TrimSpace(string(b))
		if !strings.HasPrefix(name, mapperPrefix) {
			continue
		}
		m := Mapping{Name: name}
		if slaves, _ := os.ReadDir(filepath.Join(d, "slaves")); len(slaves) > 0 {
			m.Backing = "/dev/" + slaves[0].Name()
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *Crypt) IsOpen(ctx context.Context, name string) (bool, error) {
	ms, err := c.List(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.Name == name {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 6: Run tests and lint**

Run: `go test ./pkg/crypt/ -v && golangci-lint run ./pkg/crypt/...`
Expected: PASS, no lint findings (if gosec flags `exec`-style or file-inclusion issues not covered by the `#nosec` comments, match the suppression style already used elsewhere in the repo).

- [ ] **Step 7: Commit**

```bash
git add pkg/crypt
git commit -m "crypt: LUKS2 format-on-first-use, key-slot rotation, sysfs mapping list"
```

---

### Task 5: State file `CryptDev` and crypt-aware reconciler

**Files:**
- Modify: `pkg/loop/state.go` (Mapping)
- Modify: `pkg/loop/reconcile.go`
- Modify: `pkg/loop/state_test.go`, `pkg/loop/reconcile_test.go`
- Modify: `cmd/node/main.go` (reconciler construction)

**Interfaces:**
- Consumes: `crypt.Mapping`, `crypt.New` (Task 4).
- Produces:
  ```go
  type Mapping struct { VolumeID, LoopDev, ImagePath, StagePath, CryptDev string } // CryptDev json:"cryptDev,omitempty"
  type CryptMappings interface {
      List(ctx context.Context) ([]crypt.Mapping, error)
      Close(ctx context.Context, name string) error
  }
  func NewReconciler(state *State, losetup *Losetup, cm CryptMappings, backingStorePath string) *Reconciler // cm may be nil
  ```

- [ ] **Step 1: Write failing tests**

`pkg/loop/state_test.go`:

```go
func TestStateCryptDevRoundTripAndOldFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	// A v0.4.0 state file has no cryptDev key.
	old := `{"v1":{"volumeId":"v1","loopDev":"/dev/loop0","imagePath":"/srv/v1.img","stagePath":"/s/v1"}}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p)
	if err != nil {
		t.Fatalf("LoadState old format: %v", err)
	}
	if m, _ := st.Get("v1"); m.CryptDev != "" || m.LoopDev != "/dev/loop0" {
		t.Fatalf("old entry = %+v", m)
	}
	if err := st.Put(Mapping{VolumeID: "v2", LoopDev: "/dev/loop1", ImagePath: "/srv/v2.img", StagePath: "/s/v2", CryptDev: "/dev/mapper/fbcrypt-x"}); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := st2.Get("v2"); m.CryptDev != "/dev/mapper/fbcrypt-x" {
		t.Fatalf("CryptDev not persisted: %+v", m)
	}
}
```

(Add `os` import if missing.)

`pkg/loop/reconcile_test.go` — update every existing `NewReconciler(state, NewLosetup(fake), "/srv")` to `NewReconciler(state, NewLosetup(fake), nil, "/srv")`, then add:

```go
type fakeCrypt struct {
	mappings []crypt.Mapping
	log      *[]string
}

func (f *fakeCrypt) List(context.Context) ([]crypt.Mapping, error) { return f.mappings, nil }
func (f *fakeCrypt) Close(_ context.Context, name string) error {
	*f.log = append(*f.log, "close "+name)
	return nil
}

// losetupFake lists live loops and records detaches into log.
func losetupFake(live string, log *[]string) *exectest.FakeRunner {
	fake := exectest.New()
	fake.Func = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "losetup" && len(args) > 0 && args[0] == "--json" {
			return live, nil
		}
		if name == "losetup" && len(args) > 1 && args[0] == "--detach" {
			*log = append(*log, "detach "+args[1])
			return "", nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}
	return fake
}

func TestReconcileDropsEntryWhoseCryptMappingVanished(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1", CryptDev: "/dev/mapper/fbcrypt-a"})
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/v1.img"}]}`, &log)
	rec := NewReconciler(state, NewLosetup(fake), &fakeCrypt{log: &log}, "/srv")
	if err := rec.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Get("v1"); ok {
		t.Fatal("entry kept although its crypt mapping is gone")
	}
}

func TestReconcileKeepsTrackedCryptEntry(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	_ = state.Put(Mapping{VolumeID: "v1", LoopDev: "/dev/loop0", ImagePath: "/srv/v1.img", StagePath: "/s/v1", CryptDev: "/dev/mapper/fbcrypt-a"})
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop0","back-file":"/srv/v1.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-a", Backing: "/dev/loop0"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Get("v1"); !ok || len(log) != 0 {
		t.Fatalf("tracked volume disturbed: log=%v", log)
	}
}

// Review Focus 5: a live mapping holds its loop open, so the orphan
// mapping must be closed before the loop detach is attempted.
func TestReconcileClosesOrphanCryptBeforeDetach(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop5","back-file":"/srv/v9.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-z", Backing: "/dev/loop5"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"close fbcrypt-z", "detach /dev/loop5"}
	if !slices.Equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

func TestReconcileLeavesCryptOverForeignLoop(t *testing.T) {
	state, _ := LoadState(filepath.Join(t.TempDir(), "s.json"))
	var log []string
	fake := losetupFake(`{"loopdevices":[{"name":"/dev/loop6","back-file":"/elsewhere/x.img"}]}`, &log)
	cm := &fakeCrypt{mappings: []crypt.Mapping{{Name: "fbcrypt-q", Backing: "/dev/loop6"}}, log: &log}
	if err := NewReconciler(state, NewLosetup(fake), cm, "/srv").Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("touched a foreign mapping/loop: %v", log)
	}
}
```

Imports: `fmt`, `slices`, `github.com/middlendian/fileblock-csi/pkg/crypt`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./pkg/loop/ -v`
Expected: FAIL to compile (`CryptDev`, 4-arg `NewReconciler`).

- [ ] **Step 3: Implement**

`pkg/loop/state.go` Mapping gains:

```go
	// CryptDev is the /dev/mapper path of an encrypted volume's dm-crypt
	// mapping; empty for plaintext volumes.
	CryptDev string `json:"cryptDev,omitempty"`
```

`pkg/loop/reconcile.go` (full replacement of the struct, constructor and `Reconcile`):

```go
// CryptMappings is the view of open dm-crypt mappings the reconciler
// needs; *crypt.Crypt satisfies it.
type CryptMappings interface {
	List(ctx context.Context) ([]crypt.Mapping, error)
	Close(ctx context.Context, name string) error
}

type Reconciler struct {
	state            *State
	losetup          *Losetup
	crypt            CryptMappings
	backingStorePath string
}

// NewReconciler builds a Reconciler. cm may be nil, which skips crypt
// mappings entirely.
func NewReconciler(state *State, losetup *Losetup, cm CryptMappings, backingStorePath string) *Reconciler {
	return &Reconciler{state: state, losetup: losetup, crypt: cm, backingStorePath: backingStorePath}
}

// Reconcile drops state entries whose loop device (or crypt mapping) is no
// longer what the entry says, closes fbcrypt mappings over our loops that
// no entry tracks, and then detaches loop devices backed by a .img under
// our backing store that no entry tracks. The staging mount itself is left
// alone; the kubelet retries publish/unstage.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	live, err := r.losetup.List(ctx)
	if err != nil {
		return err
	}
	liveByDev := map[string]string{} // dev -> back-file
	for _, a := range live {
		liveByDev[a.Device] = a.BackFile
	}
	var mappings []crypt.Mapping
	if r.crypt != nil {
		if mappings, err = r.crypt.List(ctx); err != nil {
			return err
		}
	}
	backingByName := map[string]string{}
	for _, m := range mappings {
		backingByName[m.Name] = m.Backing
	}

	// 1. Drop stale state entries.
	for _, m := range r.state.All() {
		back, ok := liveByDev[m.LoopDev]
		stale := !ok || back != m.ImagePath
		if m.CryptDev != "" {
			b, open := backingByName[filepath.Base(m.CryptDev)]
			stale = stale || !open || b != m.LoopDev
		}
		if stale {
			_ = r.state.Delete(m.VolumeID)
		}
	}

	trackedLoops := map[string]bool{}
	trackedCrypt := map[string]bool{}
	for _, m := range r.state.All() {
		trackedLoops[m.LoopDev] = true
		if m.CryptDev != "" {
			trackedCrypt[filepath.Base(m.CryptDev)] = true
		}
	}
	cleanRoot := filepath.Clean(r.backingStorePath)
	ours := func(dev string) bool {
		back, ok := liveByDev[dev]
		return ok && strings.HasPrefix(filepath.Clean(back), cleanRoot+string(filepath.Separator))
	}

	// 2. Close orphan crypt mappings first: an open mapping holds its loop.
	for _, m := range mappings {
		if trackedCrypt[m.Name] || !ours(m.Backing) {
			continue
		}
		_ = r.crypt.Close(ctx, m.Name)
	}

	// 3. Detach orphan loops backed by a .img under our backing store.
	for dev := range liveByDev {
		if trackedLoops[dev] || !ours(dev) {
			continue
		}
		_ = r.losetup.Detach(ctx, dev)
	}
	return nil
}
```

Add import `github.com/middlendian/fileblock-csi/pkg/crypt`.

`cmd/node/main.go`: import `github.com/middlendian/fileblock-csi/pkg/crypt`; after building `losetup`:

```go
	luks := crypt.New(exec)
	// cryptsetup takes its LUKS2 locks here and only warns without it.
	if err := os.MkdirAll("/run/cryptsetup", 0o700); err != nil {
		log.Warn("create /run/cryptsetup", "err", err)
	}
```

and `rec := loop.NewReconciler(state, losetup, luks, *storesRoot)`.

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./pkg/loop/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/loop cmd/node/main.go
git commit -m "loop: track crypt mappings in state; reconcile closes orphans before detach"
```

---

### Task 6: Controller — `encrypted` parameter

**Files:**
- Create: `pkg/driver/encryption.go`
- Modify: `pkg/driver/controller.go` (CreateVolume)
- Test: `pkg/driver/controller_test.go`

**Interfaces:**
- Consumes: `image.CreateOptions` (Task 3).
- Produces:
  ```go
  const ParamEncrypted = "encrypted"          // SC parameter and volume-context key
  const minEncryptedCapacity = 32 << 20
  func encryptedFromParams(params map[string]string) (bool, error)
  ```

- [ ] **Step 1: Write failing tests in `pkg/driver/controller_test.go`**

```go
func TestCreateVolumeEncrypted(t *testing.T) {
	c, imgs := newTestServer(t)
	params := nfsParams()
	params[ParamEncrypted] = "true"
	resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "enc",
		Parameters:         params,
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.VolumeContext[ParamEncrypted] != "true" {
		t.Fatalf("volume context = %v", resp.Volume.VolumeContext)
	}
	if !imgs.lastCreateOpts.Unformatted {
		t.Fatal("encrypted image was formatted by the controller")
	}
	// Encryption is a volume property, not a store one: the storeID in the
	// volumeID must match the plaintext config's.
	plain, _ := store.ConfigFromParams(nfsParams())
	if !strings.HasPrefix(resp.Volume.VolumeId, "fb-"+plain.StoreID()+"-") {
		t.Fatalf("volumeID %q changed store identity", resp.Volume.VolumeId)
	}
}

func TestCreateVolumePlaintextUnchanged(t *testing.T) {
	for _, v := range []string{"", "false"} {
		c, imgs := newTestServer(t)
		params := nfsParams()
		if v != "" {
			params[ParamEncrypted] = v
		}
		resp, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name: "p", Parameters: params,
			VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if _, ok := resp.Volume.VolumeContext[ParamEncrypted]; ok || imgs.lastCreateOpts.Unformatted {
			t.Fatalf("%q: ctx=%v opts=%+v", v, resp.Volume.VolumeContext, imgs.lastCreateOpts)
		}
	}
}

func TestCreateVolumeEncryptedBadValue(t *testing.T) {
	c, _ := newTestServer(t)
	params := nfsParams()
	params[ParamEncrypted] = "yes"
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "x", Parameters: params,
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

// Review Focus 3: the LUKS2 header alone is 16 MiB.
func TestCreateVolumeEncryptedTooSmallIsOutOfRange(t *testing.T) {
	c, _ := newTestServer(t)
	params := nfsParams()
	params[ParamEncrypted] = "true"
	_, err := c.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "tiny", Parameters: params,
		VolumeCapabilities: []*csi.VolumeCapability{singleNodeWriterMount()},
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 10 << 20},
	})
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("got %v, want OutOfRange", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./pkg/driver/ -run CreateVolume -v`
Expected: FAIL to compile (`ParamEncrypted` undefined).

- [ ] **Step 3: Create `pkg/driver/encryption.go`**

```go
package driver

import "fmt"

// ParamEncrypted opts a StorageClass into LUKS2 encryption. The controller
// copies it into volume context, which is how the node learns of it.
const ParamEncrypted = "encrypted"

// minEncryptedCapacity leaves room for the 16 MiB LUKS2 header plus a
// usable ext4.
const minEncryptedCapacity = 32 << 20

func encryptedFromParams(params map[string]string) (bool, error) {
	switch v := params[ParamEncrypted]; v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: must be \"true\" or \"false\"", ParamEncrypted, v)
	}
}
```

- [ ] **Step 4: Wire into `CreateVolume` in `pkg/driver/controller.go`**

After `cfg, err := store.ConfigFromParams(...)` error check:

```go
	encrypted, err := encryptedFromParams(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
```

After the `capacity` block:

```go
	if encrypted && capacity < minEncryptedCapacity {
		return nil, status.Errorf(codes.OutOfRange,
			"encrypted volumes need at least %d bytes (LUKS2 header is 16 MiB), requested %d",
			minEncryptedCapacity, capacity)
	}
```

Change the Create call and volume context:

```go
	meta, err := images.Create(ctx, volumeID, int64(capacity), image.CreateOptions{Unformatted: encrypted})
	...
	vc := cfg.ToVolumeContext()
	if encrypted {
		vc[ParamEncrypted] = "true"
	}
	vol := &csi.Volume{
		VolumeId:      meta.VolumeID,
		CapacityBytes: meta.CapacityBytes,
		VolumeContext: vc,
	}
```

The capacity check sits right after the existing capacity computation; it does not need to precede `registry.Get`.

- [ ] **Step 5: Run tests**

Run: `go test ./pkg/driver/ -v -run 'CreateVolume|ListVolumes'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/driver/encryption.go pkg/driver/controller.go pkg/driver/controller_test.go
git commit -m "driver: encrypted StorageClass parameter; controller leaves image unformatted"
```

---

### Task 7: Node — encrypted stage, unstage, expand

**Files:**
- Modify: `pkg/driver/encryption.go` (`keysFromSecrets`, `cryptStatus`)
- Modify: `pkg/driver/node.go`
- Test: `pkg/driver/node_test.go`, `pkg/driver/encryption_test.go` (create)

**Interfaces:**
- Consumes: `crypt.*` (Task 4), `loop.Mapping.CryptDev` (Task 5), `ParamEncrypted` (Task 6).
- Produces: `NodeServer.luks *crypt.Crypt` (set by `NewNodeServer` to `crypt.New(exec)`; tests replace it with `crypt.NewAt(fake, sysRoot)`).

- [ ] **Step 1: Write `pkg/driver/encryption_test.go`**

```go
package driver

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testKey = strings.Repeat("k", 44)

func TestKeysFromSecretsMissing(t *testing.T) {
	for _, s := range []map[string]string{nil, {}, {"key": ""}, {"previousKey": testKey}} {
		_, err := keysFromSecrets(s)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%v: got %v, want FailedPrecondition", s, err)
		}
		if !strings.Contains(err.Error(), "csi.storage.k8s.io/node-stage-secret-name") {
			t.Fatalf("error does not name the SC parameter: %v", err)
		}
	}
}

func TestKeysFromSecretsTooShort(t *testing.T) {
	_, err := keysFromSecrets(map[string]string{"key": "short"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
	if strings.Contains(err.Error(), "short") {
		t.Fatalf("error leaks the key: %v", err)
	}
}

// Review Focus 1: kubectl create secret --from-file keeps the trailing
// newline; the key is used exactly as stored.
func TestKeysFromSecretsPreservesBytes(t *testing.T) {
	k, err := keysFromSecrets(map[string]string{"key": testKey + "\n", "previousKey": " old \n"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.Current, []byte(testKey+"\n")) || !bytes.Equal(k.Previous, []byte(" old \n")) {
		t.Fatalf("keys altered: %q %q", k.Current, k.Previous)
	}
}
```

- [ ] **Step 2: Add node stage tests to `pkg/driver/node_test.go`**

```go
// encStage wires a NodeServer that reaches a full stage: a real .img in
// the registry's backing path, a fake runner that answers losetup,
// e2fsck, resize2fs, mount, findmnt and cryptsetup, and an empty fake
// sysfs. cryptFn overrides cryptsetup behavior; nil means "everything
// succeeds, header already formatted".
type encStage struct {
	n       *NodeServer
	fake    *exectest.FakeRunner
	vc      map[string]string
	sysRoot string
	stage   string
}

func newEncStage(t *testing.T, encrypted bool, cryptFn func(fbexec.Cmd) (string, error)) *encStage {
	t.Helper()
	fake := exectest.New()
	fake.Func = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "findmnt":
			return args[len(args)-1], nil
		case "losetup":
			if len(args) > 0 && args[0] == "--find" {
				return "/dev/loop7\n", nil
			}
		}
		return "", nil
	}
	fake.CmdFunc = func(_ context.Context, c fbexec.Cmd) (string, error) {
		if cryptFn != nil {
			return cryptFn(c)
		}
		if c.Args[0] == "luksDump" {
			return "Label:          fileblock\n", nil
		}
		return "", nil
	}
	mnt := mount.New(fake)
	reg := store.NewRegistry(t.TempDir(), nil, store.NewLocalMounter(mnt), mnt, discardLog())
	state, err := loop.LoadState(filepath.Join(t.TempDir(), "loop-mappings.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := NewNodeServer("n", fake, mnt, loop.NewLosetup(fake), state, discardLog(), reg)
	sysRoot := t.TempDir()
	n.luks = crypt.NewAt(fake, sysRoot)
	cfg := store.Config{Type: store.TypeLocal, LocalPath: t.TempDir()}
	backing, err := reg.Get(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backing, "vol-1.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vc := cfg.ToVolumeContext()
	if encrypted {
		vc[ParamEncrypted] = "true"
	}
	// Drop setup's bind mount so tests see only the stage's own calls.
	fake.Reset()
	return &encStage{n: n, fake: fake, vc: vc, sysRoot: sysRoot, stage: filepath.Join(t.TempDir(), "stage")}
}

func (e *encStage) req(secrets map[string]string) *csi.NodeStageVolumeRequest {
	r := stageReq("vol-1", e.vc)
	r.StagingTargetPath = e.stage
	r.Secrets = secrets
	return r
}

// callIndex returns the index of the first call whose name and leading
// args match, or -1.
func callIndex(calls []exectest.Call, name string, args ...string) int {
	for i, c := range calls {
		if c.Name == name && len(c.Args) >= len(args) && slices.Equal(c.Args[:len(args)], args) {
			return i
		}
	}
	return -1
}

func TestNodeStageEncryptedHappyPath(t *testing.T) {
	e := newEncStage(t, true, nil)
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey})); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	calls := e.fake.Calls
	setCap := callIndex(calls, "losetup", "--set-capacity")
	open := callIndex(calls, "cryptsetup", "open", "--type")
	if setCap < 0 || open < 0 || setCap > open {
		t.Fatalf("set-capacity must precede open: %+v", calls)
	}
	if i := callIndex(calls, "e2fsck"); i < 0 || !slices.Contains(calls[i].Args, mapper) {
		t.Fatalf("e2fsck not run on %s", mapper)
	}
	if i := callIndex(calls, "resize2fs"); i < 0 || calls[i].Args[0] != mapper {
		t.Fatalf("resize2fs not run on %s", mapper)
	}
	if i := callIndex(calls, "mount"); i < 0 || !slices.Contains(calls[i].Args, mapper) {
		t.Fatalf("mount source is not %s", mapper)
	}
	if !bytes.Equal(calls[open].Secrets[0], []byte(testKey)) {
		t.Fatal("open did not receive the key over a secret fd")
	}
	m, ok := e.n.state.Get("vol-1")
	if !ok || m.CryptDev != mapper || m.LoopDev != "/dev/loop7" {
		t.Fatalf("state = %+v", m)
	}
}

func TestNodeStageEncryptedMissingKeyTouchesNothing(t *testing.T) {
	e := newEncStage(t, true, nil)
	_, err := e.n.NodeStageVolume(context.Background(), e.req(nil))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--find") >= 0 {
		t.Fatal("attached a loop without a key")
	}
}

func TestNodeStageEncryptedAlreadyOpenRefusedBeforeAttach(t *testing.T) {
	e := newEncStage(t, true, nil)
	d := filepath.Join(e.sysRoot, "block", "dm-0")
	_ = os.MkdirAll(filepath.Join(d, "dm"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "dm", "name"), []byte(crypt.MapperName("vol-1")+"\n"), 0o644)
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--find") >= 0 {
		t.Fatal("attached a second loop for an already-open volume")
	}
}

func TestNodeStageEncryptedWrongKeyDetaches(t *testing.T) {
	e := newEncStage(t, true, func(c fbexec.Cmd) (string, error) {
		if c.Args[0] == "open" {
			return "", &fbexec.Error{Cmd: "cryptsetup", ExitCode: 2}
		}
		return "", nil
	})
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v, want PermissionDenied", err)
	}
	if callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7") < 0 {
		t.Fatal("loop not detached after wrong key")
	}
}

// Review Focus 4.
func TestNodeStageEncryptedFailureClosesMapping(t *testing.T) {
	e := newEncStage(t, true, nil)
	inner := e.fake.Func
	e.fake.Func = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "e2fsck" {
			return "", &fbexec.Error{Cmd: "e2fsck", ExitCode: 8}
		}
		return inner(ctx, name, args...)
	}
	_, err := e.n.NodeStageVolume(context.Background(), e.req(map[string]string{"key": testKey}))
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v, want Internal", err)
	}
	closeIdx := callIndex(e.fake.Calls, "cryptsetup", "close", crypt.MapperName("vol-1"))
	detachIdx := callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7")
	if closeIdx < 0 || detachIdx < 0 || closeIdx > detachIdx {
		t.Fatalf("want close then detach: %+v", e.fake.Calls)
	}
}

// The plaintext path must be byte-for-byte what it was before encryption.
func TestNodeStagePlaintextSequenceUnchanged(t *testing.T) {
	e := newEncStage(t, false, nil)
	if _, err := e.n.NodeStageVolume(context.Background(), e.req(nil)); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	var seq []string
	for _, c := range e.fake.Calls {
		if c.Name == "findmnt" {
			continue
		}
		seq = append(seq, c.Name+" "+c.Args[0])
	}
	want := []string{"losetup --find", "e2fsck -p", "losetup --set-capacity", "resize2fs /dev/loop7", "mount -t"}
	if !slices.Equal(seq, want) {
		t.Fatalf("sequence = %v, want %v", seq, want)
	}
}

func TestNodeUnstageEncryptedOrder(t *testing.T) {
	e := newEncStage(t, true, nil)
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	_ = e.n.state.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop7", ImagePath: "/x.img", StagePath: e.stage, CryptDev: mapper})
	if _, err := e.n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "vol-1", StagingTargetPath: e.stage}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	u := callIndex(e.fake.Calls, "umount")
	c := callIndex(e.fake.Calls, "cryptsetup", "close", crypt.MapperName("vol-1"))
	d := callIndex(e.fake.Calls, "losetup", "--detach", "/dev/loop7")
	if u < 0 || c < 0 || d < 0 || !(u < c && c < d) {
		t.Fatalf("want umount < close < detach, got %d %d %d", u, c, d)
	}
}

func TestNodeExpandEncryptedResizesMapper(t *testing.T) {
	e := newEncStage(t, true, nil)
	mapper := crypt.MapperPath(crypt.MapperName("vol-1"))
	_ = e.n.state.Put(loop.Mapping{VolumeID: "vol-1", LoopDev: "/dev/loop7", ImagePath: "/x.img", StagePath: e.stage, CryptDev: mapper})
	if _, err := e.n.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: "vol-1", VolumePath: "/p"}); err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	r := callIndex(e.fake.Calls, "cryptsetup", "resize", crypt.MapperName("vol-1"))
	f := callIndex(e.fake.Calls, "resize2fs", mapper)
	if r < 0 || f < 0 || r > f {
		t.Fatalf("want cryptsetup resize then resize2fs %s: %+v", mapper, e.fake.Calls)
	}
}
```

Imports to add to `node_test.go`: `bytes`, `os`, `slices`, `github.com/middlendian/fileblock-csi/pkg/crypt`. Before relying on the exact `umount`/`mount`/`e2fsck` arg shapes, read `pkg/mount/mount.go` and `pkg/image/resize.go` — `callIndex` matches by leading args, and the plaintext sequence test's `want` must reflect the real first args (`e2fsck -p`, `mount -t`); adjust `want` to the actual first arg if `Mount` builds its argv differently.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./pkg/driver/ -run 'KeysFromSecrets|NodeStageEncrypted|NodeStagePlaintext|NodeUnstageEncrypted|NodeExpandEncrypted' -v`
Expected: FAIL to compile (`keysFromSecrets`, `n.luks` undefined).

- [ ] **Step 4: Add to `pkg/driver/encryption.go`**

```go
const (
	secretKey         = "key"
	secretPreviousKey = "previousKey"
)

// keysFromSecrets reads the node-stage secret. Values are used exactly as
// stored: the README's recovery recipe feeds the same bytes to cryptsetup.
func keysFromSecrets(s map[string]string) (crypt.Keys, error) {
	cur := s[secretKey]
	if cur == "" {
		return crypt.Keys{}, status.Errorf(codes.FailedPrecondition,
			"encrypted volume needs a %q entry in its node-stage secret; set "+
				"csi.storage.k8s.io/node-stage-secret-name and "+
				"csi.storage.k8s.io/node-stage-secret-namespace on the StorageClass", secretKey)
	}
	if len(cur) < crypt.MinKeyLen {
		return crypt.Keys{}, status.Errorf(codes.InvalidArgument,
			"node-stage secret %q is %d bytes; at least %d are required", secretKey, len(cur), crypt.MinKeyLen)
	}
	return crypt.Keys{Current: []byte(cur), Previous: []byte(s[secretPreviousKey])}, nil
}

func cryptStatus(err error) error {
	switch {
	case errors.Is(err, crypt.ErrWrongKey):
		return status.Errorf(codes.PermissionDenied, "%v", err)
	case errors.Is(err, crypt.ErrNotBlank):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return status.Errorf(codes.Internal, "luks (needs cryptsetup and the dm_crypt kernel module): %v", err)
	}
}
```

Imports: `errors`, `fmt`, `crypt`, `codes`, `status`.

- [ ] **Step 5: Modify `pkg/driver/node.go`**

Field in `NodeServer`: `luks *crypt.Crypt`; in `NewNodeServer`: `luks: crypt.New(exec),`. Import `path/filepath` and `pkg/crypt`.

In `NodeStageVolume`, after the `ConfigFromVolumeContext` error check:

```go
	encrypted := req.GetVolumeContext()[ParamEncrypted] == "true"
	var keys crypt.Keys
	if encrypted {
		if keys, err = keysFromSecrets(req.GetSecrets()); err != nil {
			return nil, err
		}
	}
```

After the image `os.Stat` block and before `// 1. Attach a loop device.`:

```go
	mapper := crypt.MapperName(volumeID)
	if encrypted {
		// Attach always takes a fresh loop, so an open mapping belongs to
		// another attachment of this .img: a second one would be two ext4
		// instances over one file (#42).
		open, err := n.luks.IsOpen(ctx, mapper)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list dm-crypt mappings: %v", err)
		}
		if open {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is already open on this node as %s; refusing a second mapping of one image",
				volumeID, crypt.MapperPath(mapper))
		}
	}
```

Replace steps 2–5 (from `// 2. e2fsck` to the state `Put`) with:

```go
	target, cryptDev := dev, ""
	if encrypted {
		// Grow the loop first so the mapping opens at the expanded size.
		if err := n.losetup.SetCapacity(ctx, dev); err != nil {
			detachOnFail()
			return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
		}
		cryptDev, err = n.luks.Prepare(ctx, dev, mapper, keys)
		if err != nil {
			detachOnFail()
			return nil, cryptStatus(err)
		}
		target = cryptDev
		detachOnFail = func() {
			_ = n.luks.Close(ctx, mapper)
			_ = n.losetup.Detach(ctx, dev)
		}
	}

	// 2. e2fsck always (-p is a no-op on clean fs).
	if err := image.Fsck(ctx, n.exec, target); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "fsck: %v", err)
	}

	// 3. If the image was expanded since the last stage, grow the fs now.
	//    resize2fs is a no-op when the fs already fills the device.
	if !encrypted {
		if err := n.losetup.SetCapacity(ctx, dev); err != nil {
			detachOnFail()
			return nil, status.Errorf(codes.Internal, "set capacity: %v", err)
		}
	}
	if err := image.Resize2fs(ctx, n.exec, target); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}

	// 4. Mount.
	if err := os.MkdirAll(stagePath, 0o755); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mkdir stage: %v", err)
	}
	mountOpts := mountOptionsFromCap(req.GetVolumeCapability())
	if err := n.mnt.Mount(ctx, target, stagePath, image.DefaultFs, mountOpts); err != nil {
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}

	// 5. Persist mapping.
	if err := n.state.Put(loop.Mapping{
		VolumeID:  volumeID,
		LoopDev:   dev,
		ImagePath: imgPath,
		StagePath: stagePath,
		CryptDev:  cryptDev,
	}); err != nil {
		_ = n.mnt.Unmount(ctx, stagePath)
		detachOnFail()
		return nil, status.Errorf(codes.Internal, "persist state: %v", err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
```

`detachOnFail` must be declared with `detachOnFail := func() {...}` as today so it can be reassigned.

`NodeUnstageVolume` state block:

```go
	if m, ok := n.state.Get(volumeID); ok {
		if m.CryptDev != "" {
			if err := n.luks.Close(ctx, filepath.Base(m.CryptDev)); err != nil {
				return nil, status.Errorf(codes.Internal, "cryptsetup close: %v", err)
			}
		}
		if err := n.losetup.Detach(ctx, m.LoopDev); err != nil {
			return nil, status.Errorf(codes.Internal, "losetup --detach: %v", err)
		}
		_ = n.state.Delete(volumeID)
	}
```

`NodeExpandVolume`, replacing the `Resize2fs` call:

```go
	target := m.LoopDev
	if m.CryptDev != "" {
		if err := n.luks.Resize(ctx, filepath.Base(m.CryptDev)); err != nil {
			return nil, status.Errorf(codes.Internal, "cryptsetup resize: %v", err)
		}
		target = m.CryptDev
	}
	if err := image.Resize2fs(ctx, n.exec, target); err != nil {
		return nil, status.Errorf(codes.Internal, "resize2fs: %v", err)
	}
```

- [ ] **Step 6: Run the full driver suite and lint**

Run: `go test ./pkg/driver/ -v && make fmt-check vet lint`
Expected: PASS, no findings.

- [ ] **Step 7: Commit**

```bash
git add pkg/driver
git commit -m "driver: stage, unstage and expand encrypted volumes through dm-crypt"
```

---

### Task 8: Packaging, CI host deps, smoke and sanity

**Files:**
- Modify: `Dockerfile`, `.github/workflows/ci.yml`, `hack/smoke.sh`, `hack/csi-sanity.sh`

**Interfaces:**
- Consumes: the node/controller binaries from Tasks 1–7.

- [ ] **Step 1: Dockerfile**

Change the comment above `RUN apt-get` to also name `cryptsetup-bin (cryptsetup, for encrypted volumes)` and add `cryptsetup-bin` to the package list:

```dockerfile
        e2fsprogs util-linux ca-certificates nfs-common netbase cryptsetup-bin \
```

Add a build-time assertion after the netbase one:

```dockerfile
RUN command -v cryptsetup >/dev/null \
 || (echo "ERROR: cryptsetup missing — install cryptsetup-bin" >&2 && exit 1)
```

- [ ] **Step 2: CI host deps**

`.github/workflows/ci.yml` "Install host dependencies": `sudo apt-get install -y e2fsprogs util-linux git cryptsetup-bin` and add `sudo modprobe dm_crypt` on the next line.

- [ ] **Step 3: Smoke cleanup closes our crypt mappings**

In `hack/smoke.sh` `cleanup()`, before the `losetup --json --list` loop:

```bash
  for m in /dev/mapper/fbcrypt-*; do
    [[ -e "$m" ]] || continue
    dev=$(cryptsetup status "$(basename "$m")" 2>/dev/null | awk '/device:/ {print $2}')
    back=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null || true)
    case "$back" in "$STORES"/*) DM_DISABLE_UDEV=1 cryptsetup close "$(basename "$m")" ;; esac
  done
```

Also update the header comment's prereq list to include `cryptsetup` and `openssl`.

- [ ] **Step 4: Smoke encrypted block**

Insert before `echo; echo "smoke OK"` (the node is running as `node-b` at that point; restart it as `local` first):

```bash
echo "::: encrypted volume: first-stage format, ciphertext at rest, rotation"
restart_node() {
  kill "$NODE_PID"; wait "$NODE_PID" 2>/dev/null || true
  "$BIN/fileblock-node" \
    --endpoint="unix://$NODE_SOCK" \
    --node-id=local \
    --state-dir="$STATE" \
    --stores-root="$STORES" \
    --log-level=debug >>"$LOG/node.log" 2>&1 &
  NODE_PID=$!
  for _ in $(seq 1 20); do [[ -S "$NODE_SOCK" ]] && break; sleep 0.1; done
}
restart_node
# Hex keys: csc parses X_CSI_SECRETS as k=v pairs and base64 padding is '='.
KEY1=$(openssl rand -hex 32)
KEY2=$(openssl rand -hex 32)
CREATE_OUT=$(csc controller new \
  --cap "SINGLE_NODE_WRITER,mount,ext4" \
  --req-bytes $((128*1024*1024)) \
  --params "backingStore.type=local" \
  --params "backingStore.local.path=$BACKING" \
  --params "encrypted=true" \
  enc-vol)
VOL4=$(printf '%s\n' "$CREATE_OUT" | head -n1 | awk '{print $1}' | tr -d '"')
IMG4="$BACKING/$VOL4.img"
STAGE4="$STATE/staging/$VOL4"
mkdir -p "$STAGE4"
stage_enc() {
  X_CSI_SECRETS="$1" CSI_ENDPOINT="unix://$NODE_SOCK" csc node stage \
    --cap "SINGLE_NODE_WRITER,mount,ext4" \
    --staging-target-path "$STAGE4" \
    --vol-context "backingStore.type=local" \
    --vol-context "backingStore.local.path=$BACKING" \
    --vol-context "encrypted=true" \
    "$VOL4"
}
unstage_enc() {
  CSI_ENDPOINT="unix://$NODE_SOCK" csc node unstage --staging-target-path "$STAGE4" "$VOL4"
}
cmp -s <(head -c 16777216 "$IMG4") <(head -c 16777216 /dev/zero) \
  || { echo "controller wrote to an encrypted image"; exit 1; }
stage_enc "key=$KEY1"
cryptsetup isLuks "$IMG4" || { echo "image is not LUKS after first stage"; exit 1; }
findmnt -no FSTYPE "$STAGE4" | grep -q '^ext4$' || { echo "encrypted fs not ext4"; exit 1; }
CANARY="fileblock-smoke-canary-$(openssl rand -hex 8)"
echo "$CANARY" >"$STAGE4/canary"
unstage_enc
grep -qa "$CANARY" "$IMG4" && { echo "plaintext canary found in the .img"; exit 1; }

echo "::: rotation: key=KEY2, previousKey=KEY1"
stage_enc "key=$KEY2,previousKey=$KEY1"
grep -qx "$CANARY" "$STAGE4/canary" || { echo "data lost across rotation"; exit 1; }
unstage_enc
printf %s "$KEY1" | cryptsetup open --test-passphrase --key-file=- "$IMG4" \
  && { echo "old key still opens after rotation"; exit 1; }
printf %s "$KEY2" | cryptsetup open --test-passphrase --key-file=- "$IMG4" \
  || { echo "new key does not open after rotation"; exit 1; }

echo "::: wrong key is refused and leaves nothing attached"
stage_enc "key=$(openssl rand -hex 32)" && { echo "stage with a wrong key succeeded"; exit 1; }
losetup --noheadings --output BACK-FILE | grep -qF "$IMG4" \
  && { echo "loop left attached after wrong-key stage"; exit 1; }

echo "::: orphan crypt mapping is reclaimed on plugin restart"
ORPHAN4=$(losetup --find --show "$IMG4")
printf %s "$KEY2" | DM_DISABLE_UDEV=1 cryptsetup open --key-file=- "$ORPHAN4" fbcrypt-smokeorphan
restart_node
sleep 1
[[ -e /dev/mapper/fbcrypt-smokeorphan ]] && { echo "orphan crypt mapping not closed"; exit 1; }
losetup "$ORPHAN4" 2>/dev/null && { echo "orphan loop under crypt not detached"; exit 1; } || true
csc controller del "$VOL4"
```

- [ ] **Step 5: csi-sanity encrypted pass**

`hack/csi-sanity.sh`: add the same `fbcrypt-*` close loop (Step 3) to `cleanup()` before its losetup loop. After the existing `csi-sanity` invocation append:

```bash
echo "::: csi-sanity (encrypted)"
csi-sanity \
  --csi.controllerendpoint="unix://$CTL_SOCK" \
  --csi.endpoint="unix://$NODE_SOCK" \
  --csi.testvolumeparameters=<(printf "backingStore.type: local\nbackingStore.local.path: %s\nencrypted: \"true\"\n" "$BACKING") \
  --csi.secrets=<(printf "NodeStageVolumeSecret:\n  key: %s\n" "$(openssl rand -hex 32)") \
  --csi.testvolumesize=$((128*1024*1024))
```

- [ ] **Step 6: Local gates, then CI**

Run locally: `bash -n hack/smoke.sh hack/csi-sanity.sh && make fmt-check vet lint tidy-check test build`
Expected: PASS. Smoke/sanity need root + loop + dm-crypt, unavailable in this container: push and confirm the `make check` job in `ci.yml` is green (`gh pr checks 43 --watch`). If smoke fails in `cryptsetup open` with a udev/cookie hang or `/run/cryptsetup` error, that is spec §2's container assumption failing — stop and report rather than papering over it.

- [ ] **Step 7: Commit**

```bash
git add Dockerfile .github/workflows/ci.yml hack/smoke.sh hack/csi-sanity.sh
git commit -m "build: ship cryptsetup; smoke and csi-sanity cover encrypted volumes"
```

---

### Task 9: End-to-end encrypted volume test

**Files:**
- Modify: `hack/e2e.sh`
- Create: `test/e2e/encryption_test.go`

**Interfaces:**
- Consumes: e2e helpers `makeNamespace`, `applyYAML`, `kubectl`, `kubectlRaw`, `waitPodReady`, `waitPodGone`, `pvcManifestSC`, `podWithScript`, `pvForPVC`, `defaultPodReady`; env `E2E_BACKING_KIND`, `E2E_BACKING_HOST`, `NFS_EXPORT`.

- [ ] **Step 1: `hack/e2e.sh` host prerequisites**

After `$SUDO modprobe loop 2>/dev/null || true` add:

```bash
# Encrypted volumes: the node plugin needs dm_crypt in the shared kernel;
# the e2e test inspects .img headers on the runner with cryptsetup.
$SUDO modprobe dm_crypt 2>/dev/null || true
if ! command -v cryptsetup >/dev/null; then
  log "installing cryptsetup-bin (one-shot)"
  $SUDO apt-get update -qq
  $SUDO DEBIAN_FRONTEND=noninteractive apt-get install -y -qq cryptsetup-bin
fi
```

- [ ] **Step 2: Write `test/e2e/encryption_test.go`**

```go
//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestEncryptedVolumeRotation drives the whole encrypted lifecycle through
// the kubelet: the provisioner resolves ${pvc.namespace} in the secret
// namespace, the node formats on first stage, only ciphertext reaches the
// backing store, and a Secret change rotates the key slot on next stage.
func TestEncryptedVolumeRotation(t *testing.T) {
	ns := makeNamespace(t)
	const scName = "fileblock-encrypted"
	key1, key2 := randomKey(t), randomKey(t)

	applyYAML(t, luksSecretYAML(ns, key1, ""))
	applyYAML(t, encryptedSCYAML(t, scName))
	t.Cleanup(func() { _, _ = kubectlRaw("delete", "sc", scName, "--ignore-not-found") })

	applyYAML(t, pvcManifestSC(ns, "vol", "128Mi", scName))
	canary := "fileblock-e2e-canary-" + key1[:16]
	applyYAML(t, podWithScript(ns, "writer", "vol", fmt.Sprintf("set -eu\necho %s > /data/canary\nsync\nsleep 3600\n", canary)))
	waitPodReady(t, ns, "writer", defaultPodReady)
	img := imagePath(t, pvForPVC(t, ns, "vol"))
	kubectl(t, "-n", ns, "delete", "pod", "writer", "--wait=true")
	waitPodGone(t, ns, "writer", 60*time.Second)

	// 1. Only ciphertext on the backing store.
	if err := sudo(nil, "cryptsetup", "isLuks", img); err != nil {
		t.Fatalf("%s is not a LUKS volume: %v", img, err)
	}
	if err := sudo(nil, "grep", "-qa", canary, img); err == nil {
		t.Fatalf("plaintext canary found in %s", img)
	}

	// 2. Rotate: the next stage moves the slot from key1 to key2.
	applyYAML(t, luksSecretYAML(ns, key2, key1))
	applyYAML(t, podWithScript(ns, "reader", "vol", fmt.Sprintf("set -eu\ngrep -qx %s /data/canary\nsleep 3600\n", canary)))
	waitPodReady(t, ns, "reader", defaultPodReady)
	kubectl(t, "-n", ns, "delete", "pod", "reader", "--wait=true")
	waitPodGone(t, ns, "reader", 60*time.Second)

	// 3. The old key no longer opens the header; the new one does.
	if opensWith(t, img, key1) {
		t.Fatal("old key still opens the volume after rotation")
	}
	if !opensWith(t, img, key2) {
		t.Fatal("new key does not open the volume after rotation")
	}
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func luksSecretYAML(ns, key, previous string) string {
	prev := ""
	if previous != "" {
		prev = fmt.Sprintf("  previousKey: %q\n", previous)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: fileblock-luks
  namespace: %s
stringData:
  key: %q
%s`, ns, key, prev)
}

// encryptedSCYAML clones the default `fileblock` StorageClass's backing
// store, so the test runs against whichever store the variant set up.
func encryptedSCYAML(t *testing.T, name string) string {
	t.Helper()
	raw := kubectl(t, "get", "sc", "fileblock", "-o", "jsonpath={.parameters}")
	params := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("parse fileblock SC parameters %q: %v", raw, err)
	}
	params["encrypted"] = "true"
	params["csi.storage.k8s.io/node-stage-secret-name"] = "fileblock-luks"
	params["csi.storage.k8s.io/node-stage-secret-namespace"] = "${pvc.namespace}"
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, params[k])
	}
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: fileblock.csi
parameters:
%sreclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
`, name, b.String())
}

// imagePath is where the runner sees a volume's .img: the NFS export
// itself in nfs mode, the shared host directory in local mode.
func imagePath(t *testing.T, handle string) string {
	t.Helper()
	dir := os.Getenv("E2E_BACKING_HOST")
	if os.Getenv("E2E_BACKING_KIND") == "nfs" {
		dir = os.Getenv("NFS_EXPORT")
	}
	if dir == "" {
		t.Skip("backing store path not exported by hack/e2e.sh")
	}
	return filepath.Join(dir, handle+".img")
}

// sudo runs a command as root on the runner: .img files are 0600 root.
func sudo(stdin []byte, name string, args ...string) error {
	cmd := exec.Command("sudo", append([]string{name}, args...)...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	return cmd.Run()
}

func opensWith(t *testing.T, img, key string) bool {
	t.Helper()
	err := sudo([]byte(key), "cryptsetup", "open", "--test-passphrase", "--key-file=-", img)
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 2 {
		return false
	}
	t.Fatalf("cryptsetup --test-passphrase %s: %v", img, err)
	return false
}
```

- [ ] **Step 3: Compile check locally**

Run: `go vet -tags=e2e ./test/e2e/ && bash -n hack/e2e.sh`
Expected: PASS.

- [ ] **Step 4: Commit and verify in CI**

```bash
git add hack/e2e.sh test/e2e/encryption_test.go
git commit -m "e2e: encrypted volume format, ciphertext-at-rest and rotation"
git push
```

Watch `gh pr checks 43 --watch`: all three `e2e` variants (local, nfs v4.1, nfs v3) must pass `TestEncryptedVolumeRotation` and the renamed-token `TestNFSSubDirPerNamespace`. On failure, read the job log's `dump_state` output (node logs carry the cryptsetup output) before changing code.

---

### Task 10: Documentation and CHANGELOG

**Files:**
- Modify: `README.md`, `CLAUDE.md`, `CHANGELOG.md`
- Create: `examples/storageclass-encrypted.yaml`

- [ ] **Step 1: `examples/storageclass-encrypted.yaml`**

```yaml
# Encrypted fileblock StorageClass. Each namespace keeps its own key in a
# Secret named fileblock-luks; the kubelet hands it to the node plugin at
# stage time. Generate a key with: openssl rand -base64 32
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock-encrypted
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: nfs.example.internal
  backingStore.nfs.path: /exports/fileblock
  backingStore.nfs.mountOptions: "nfsvers=4.1,hard,timeo=600"
  encrypted: "true"
  csi.storage.k8s.io/node-stage-secret-name: fileblock-luks
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: v1
kind: Secret
metadata:
  name: fileblock-luks
  namespace: default
stringData:
  key: "REPLACE-WITH-openssl-rand-base64-32"
```

- [ ] **Step 2: README**

1. Add a `## Encryption` section after `## Configuration` covering, in this order: what it protects (the `.img` at rest, snapshots, backups) and what it doesn't (data while staged, anyone who can read the Secret); the StorageClass + Secret example (point at `examples/storageclass-encrypted.yaml`); the token table:

   | token | `subDir` | node-stage-secret-name | node-stage-secret-namespace |
   |---|---|---|---|
   | `${pvc.namespace}` | yes | yes | yes |
   | `${pvc.name}` | yes | yes | no |
   | `${pv.name}` | yes | yes | yes |

   Key rules: at least 32 bytes, used byte-for-byte (a trailing newline from `--from-file` is part of the key). Rotation: set `previousKey` to the current key and `key` to a new one; each volume moves on its next stage (pod restart/reschedule); remove `previousKey` once every volume has restaged. The snapshot caveat: rotation changes the key slot, not the master key; old `.img` copies still open with old keys; after a real compromise copy the data into a fresh encrypted volume. Key backup: keep a copy outside the cluster; a lost key is lost data. Recovery:
   ```sh
   kubectl -n <ns> get secret fileblock-luks -o jsonpath='{.data.key}' | base64 -d \
     | sudo cryptsetup open --key-file=- /path/to/fb-….img recovered
   sudo mount /dev/mapper/recovered /mnt
   ```
   Overhead: 16 MiB LUKS2 header inside the requested size; encrypted volumes must be at least 32 MiB. Nodes need the `dm_crypt` kernel module. Existing plaintext volumes are not converted.
2. Configuration table: add rows `encrypted` (no; `"true"` enables LUKS2, see Encryption) and `csi.storage.k8s.io/node-stage-secret-name` / `-namespace` (when encrypted; Secret holding `key`).
3. `## Limitations`: add "Encryption applies to new volumes only; key rotation lands on next stage, not live; master-key re-encryption is not supported."

- [ ] **Step 3: CLAUDE.md**

- Repo map: add `pkg/crypt/            LUKS2 via cryptsetup: format-on-first-stage, key-slot rotation, dm mapping list`.
- On-disk contract: after "`pkg/image` is the **only** package that writes `.img` files" add: "— it alone creates, truncates and deletes them. `pkg/crypt` writes *inside* an encrypted image, through the loop device, exactly as `e2fsck`/`resize2fs` already do: the LUKS2 header and, inside the mapping, ext4. Encrypted images are created unformatted (all zeros); the node formats them on first stage."
- State file invariants: document `cryptDev` (optional `/dev/mapper/fbcrypt-…` path) and add invariant 4: "Every `fbcrypt-*` mapping over a loop backed by our store and absent from the state file is closed on plugin start, before orphan loops are detached."
- Conventions: add "Secrets reach child processes only via `exec.Cmd.Secrets` (`/dev/fd/N`), never argv." and "`crypt.ErrWrongKey` → `PermissionDenied`; `crypt.ErrNotBlank` → `FailedPrecondition`."
- CSI surface: add "Encryption: opt-in via SC `encrypted: \"true\"` + node-stage secret (`key`, optional `previousKey`)."

- [ ] **Step 4: CHANGELOG `[Unreleased]`**

```markdown
### Added

- Optional LUKS2 encryption. A StorageClass with `encrypted: "true"` and
  the standard `csi.storage.k8s.io/node-stage-secret-name` /
  `-namespace` parameters gets volumes whose `.img` is only ever
  ciphertext on the backing store. The node formats on first stage and
  never overwrites a header it cannot open. Rotation: put the old key in
  the Secret's `previousKey` and the new one in `key`; each volume moves
  its key slot on its next stage. The runtime image now includes
  `cryptsetup-bin`; nodes need the `dm_crypt` kernel module. No RBAC
  change — the kubelet reads the Secret.

### Changed

- **Breaking:** `backingStore.nfs.subDir` tokens are now spelled
  `${pvc.namespace}`, `${pvc.name}` and `${pv.name}` — the spelling
  external-provisioner uses for the node-stage secret parameters, so a
  StorageClass uses one vocabulary throughout. The `${pvc.metadata.*}`
  spelling is no longer accepted. Existing volumes are unaffected (subDir
  is resolved once, at creation); a StorageClass still using the old
  tokens fails new provisioning with `InvalidArgument`. StorageClass
  parameters are immutable: delete and recreate the class under the same
  name with the new tokens — bound PVCs are unaffected.
```

- [ ] **Step 5: Verify and commit**

Run: `make fmt-check vet lint tidy-check test build`
Expected: PASS.

```bash
git add README.md CLAUDE.md CHANGELOG.md examples/storageclass-encrypted.yaml
git commit -m "docs: encryption guide, subDir token change, CLAUDE.md contract updates"
```

---

## Self-Review Notes

- Spec coverage: Secret contract → Task 7; RunCmd → Task 2; pkg/crypt (MapperName, label, blank check, rotation, pbkdf, udev env, --disable-keyring) → Task 4; controller → Tasks 3, 6; node incl. #42 guard, set-capacity ordering, unstage, expand → Task 7; state/reconcile → Task 5; Dockerfile, no RBAC → Task 8; subDir spelling → Task 1; unit/smoke/sanity/e2e → Tasks 2–9; docs → Task 10.
- Additions beyond the spec, justified in Review Focus: 32 MiB encrypted minimum (`OutOfRange`); `crypt.Resize` in NodeExpandVolume (spec says resize2fs targets the mapper; resize makes that correct if expansion ever runs while staged).
- The spec's `RunWithSecrets(ctx, secrets, name, args...)` is realized as `RunCmd(ctx, Cmd{…Secrets})` because cryptsetup also needs `Env`; same fd contract.

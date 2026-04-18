// Command clawmast-release is the internal release tool.
//
// It builds, signs, and uploads ClawMast release artefacts. Invoked by
// the GitHub Actions release workflow and never shipped to end users.
//
// Iteration 2 (T2-01) wires in two subcommands so the update-channel
// smoke test can sign a test manifest against a throwaway keypair
// without shelling out to the `minisign` CLI:
//
//	clawmast-release keygen -pub <path>
//	    Generate a fresh Ed25519 minisign keypair. The public key is
//	    written to <path>. The private key is written to stdout in
//	    minisign's text form so callers can capture it into a shell
//	    variable without it ever hitting disk.
//
//	clawmast-release sign -key <privkey-path> -in <file> -out <sig>
//	    Sign <file> with the minisign private key read from
//	    <privkey-path>. The signature file is the .minisig form that
//	    the updater client expects at <channel>/manifest.json.minisig.
//
// Full release-pipeline wiring (build matrices, uploads, changelog
// generation) lands in a later T2-xx task.
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"

	"aead.dev/minisign"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		if err := runKeygen(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "sign":
		if err := runSign(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "build":
		if err := runBuild(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "manifest":
		if err := runManifest(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "clawmast-release: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	pubPath := fs.String("pub", "", "path to write the public key (required)")
	_ = fs.Parse(args)
	if *pubPath == "" {
		return fmt.Errorf("-pub is required")
	}
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}
	if err := os.WriteFile(*pubPath, pubText, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *pubPath, err)
	}
	privText, err := priv.MarshalText()
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	// Private key on stdout — callers capture it into a variable and
	// keep it in memory only. It is never written to disk by this tool.
	_, err = os.Stdout.Write(privText)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout); err != nil {
		return err
	}
	return nil
}

func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "path to minisign private key file (required)")
	inPath := fs.String("in", "", "path to file to sign (required)")
	outPath := fs.String("out", "", "path to write signature (required)")
	comment := fs.String("comment", "", "trusted comment embedded in the signature")
	_ = fs.Parse(args)
	if *keyPath == "" || *inPath == "" || *outPath == "" {
		return fmt.Errorf("-key, -in, and -out are required")
	}
	privBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", *keyPath, err)
	}
	if minisign.IsEncrypted(privBytes) {
		return fmt.Errorf("%s is password-encrypted; decrypt it first", *keyPath)
	}
	var priv minisign.PrivateKey
	if err := priv.UnmarshalText(privBytes); err != nil {
		return fmt.Errorf("parse %s: %w", *keyPath, err)
	}
	in, err := os.Open(*inPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", *inPath, err)
	}
	defer in.Close()
	body, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read %s: %w", *inPath, err)
	}
	var sig []byte
	if *comment == "" {
		sig = minisign.Sign(priv, body)
	} else {
		sig = minisign.SignWithComments(priv, body, *comment, "clawmast-release")
	}
	if err := os.WriteFile(*outPath, sig, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *outPath, err)
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: clawmast-release <subcommand> [flags]

subcommands:
  keygen   -pub <file>
                                   generate a fresh minisign keypair
  sign     -key <privkey> -in <file> -out <sig> [-comment <str>]
                                   sign <file> with the given key
  build    -version <vX.Y.Z> [-out dist] [-commit <sha>] [-platforms <list>]
                                   cross-compile worker tarballs for the
                                   release matrix
  manifest -version <vX.Y.Z> -base-url <url> -out <path>
                                   [-dir dist] [-channel stable] [-notes <str>]
                                   build a manifest.json referencing the
                                   tarballs under -dir

The private key for keygen is printed to stdout; hold it in memory
only. See architecture/refactor.md §10 decision #6 for key custody.
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "clawmast-release:", err)
	os.Exit(1)
}

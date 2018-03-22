// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package csource generates [almost] equivalent C programs from syzkaller programs.
package csource

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys/targets"
)

func Write(p *prog.Prog, opts Options) ([]byte, error) {
	if err := opts.Check(); err != nil {
		return nil, fmt.Errorf("csource: invalid opts: %v", err)
	}
	ctx := &context{
		p:         p,
		opts:      opts,
		target:    p.Target,
		sysTarget: targets.List[p.Target.OS][p.Target.Arch],
		w:         new(bytes.Buffer),
		calls:     make(map[string]uint64),
	}

	calls, vars, err := ctx.generateProgCalls(ctx.p)
	if err != nil {
		return nil, err
	}

	mmapProg := p.Target.GenerateUberMmapProg()
	mmapCalls, _, err := ctx.generateProgCalls(mmapProg)
	if err != nil {
		return nil, err
	}

	for _, c := range append(mmapProg.Calls, p.Calls...) {
		ctx.calls[c.Meta.CallName] = c.Meta.NR
	}

	ctx.print("// autogenerated by syzkaller (http://github.com/google/syzkaller)\n\n")

	hdr, err := createCommonHeader(p, opts)
	if err != nil {
		return nil, err
	}
	ctx.w.Write(hdr)
	ctx.print("\n")

	ctx.generateSyscallDefines()

	if len(vars) != 0 {
		ctx.printf("uint64_t r[%v] = {", len(vars))
		for i, v := range vars {
			if i != 0 {
				ctx.printf(", ")
			}
			ctx.printf("0x%x", v)
		}
		ctx.printf("};\n")
	}
	if opts.Procs > 1 || opts.EnableCgroups {
		ctx.printf("unsigned long long procid;\n")
	}

	if !opts.Repeat {
		ctx.generateTestFunc(calls, len(vars) != 0, "loop")

		ctx.print("int main()\n{\n")
		for _, c := range mmapCalls {
			ctx.printf("%s", c)
		}
		if opts.HandleSegv {
			ctx.printf("\tinstall_segv_handler();\n")
		}
		if opts.UseTmpDir {
			ctx.printf("\tuse_temporary_dir();\n")
		}
		if opts.Sandbox != "" {
			ctx.printf("\tint pid = do_sandbox_%v();\n", opts.Sandbox)
			ctx.print("\tint status = 0;\n")
			ctx.print("\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
		} else {
			if opts.EnableTun {
				ctx.printf("\tinitialize_tun();\n")
				ctx.printf("\tinitialize_netdevices();\n")
			}
			ctx.print("\tloop();\n")
		}
		ctx.print("\treturn 0;\n}\n")
	} else {
		ctx.generateTestFunc(calls, len(vars) != 0, "execute_one")
		if opts.Procs <= 1 {
			ctx.print("int main()\n{\n")
			for _, c := range mmapCalls {
				ctx.printf("%s", c)
			}
			if opts.HandleSegv {
				ctx.print("\tinstall_segv_handler();\n")
			}
			if opts.UseTmpDir {
				ctx.print("\tchar *cwd = get_current_dir_name();\n")
			}
			ctx.print("\tfor (;;) {\n")
			if opts.UseTmpDir {
				ctx.print("\t\tif (chdir(cwd))\n")
				ctx.print("\t\t\tfail(\"failed to chdir\");\n")
				ctx.print("\t\tuse_temporary_dir();\n")
			}
			if opts.Sandbox != "" {
				ctx.printf("\t\tint pid = do_sandbox_%v();\n", opts.Sandbox)
				ctx.print("\t\tint status = 0;\n")
				ctx.print("\t\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			} else {
				if opts.EnableTun {
					ctx.printf("\t\tinitialize_tun();\n")
					ctx.printf("\t\tinitialize_netdevices();\n")
				}
				ctx.print("\t\tloop();\n")
			}
			ctx.print("\t}\n}\n")
		} else {
			ctx.print("int main()\n{\n")
			for _, c := range mmapCalls {
				ctx.printf("%s", c)
			}
			if opts.UseTmpDir {
				ctx.print("\tchar *cwd = get_current_dir_name();\n")
			}
			ctx.printf("\tfor (procid = 0; procid < %v; procid++) {\n", opts.Procs)
			ctx.print("\t\tif (fork() == 0) {\n")
			if opts.HandleSegv {
				ctx.print("\t\t\tinstall_segv_handler();\n")
			}
			ctx.print("\t\t\tfor (;;) {\n")
			if opts.UseTmpDir {
				ctx.print("\t\t\t\tif (chdir(cwd))\n")
				ctx.print("\t\t\t\t\tfail(\"failed to chdir\");\n")
				ctx.print("\t\t\t\tuse_temporary_dir();\n")
			}
			if opts.Sandbox != "" {
				ctx.printf("\t\t\t\tint pid = do_sandbox_%v();\n", opts.Sandbox)
				ctx.print("\t\t\t\tint status = 0;\n")
				ctx.print("\t\t\t\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			} else {
				if opts.EnableTun {
					ctx.printf("\t\t\t\tinitialize_tun();\n")
					ctx.printf("\t\t\t\tinitialize_netdevices();\n")
				}
				ctx.print("\t\t\t\tloop();\n")
			}
			ctx.print("\t\t\t}\n")
			ctx.print("\t\t}\n")
			ctx.print("\t}\n")
			ctx.print("\tsleep(1000000);\n")
			ctx.print("\treturn 0;\n}\n")
		}
	}

	// Remove NONFAILING and debug calls.
	result := ctx.w.Bytes()
	if !opts.HandleSegv {
		re := regexp.MustCompile(`\t*NONFAILING\((.*)\);\n`)
		result = re.ReplaceAll(result, []byte("$1;\n"))
	}
	if !opts.Debug {
		re := regexp.MustCompile(`\t*debug\(.*\);\n`)
		result = re.ReplaceAll(result, nil)
		re = regexp.MustCompile(`\t*debug_dump_data\(.*\);\n`)
		result = re.ReplaceAll(result, nil)
	}
	result = bytes.Replace(result, []byte("NORETURN"), nil, -1)
	result = bytes.Replace(result, []byte("PRINTF"), nil, -1)

	// Remove duplicate new lines.
	for {
		result1 := bytes.Replace(result, []byte{'\n', '\n', '\n'}, []byte{'\n', '\n'}, -1)
		result1 = bytes.Replace(result1, []byte("\n\n#include"), []byte("\n#include"), -1)
		if len(result1) == len(result) {
			break
		}
		result = result1
	}

	return result, nil
}

type context struct {
	p         *prog.Prog
	opts      Options
	target    *prog.Target
	sysTarget *targets.Target
	w         *bytes.Buffer
	calls     map[string]uint64 // CallName -> NR
}

func (ctx *context) print(str string) {
	ctx.w.WriteString(str)
}

func (ctx *context) printf(str string, args ...interface{}) {
	ctx.print(fmt.Sprintf(str, args...))
}

func (ctx *context) generateTestFunc(calls []string, hasVars bool, name string) {
	opts := ctx.opts
	if !opts.Threaded && !opts.Collide {
		ctx.printf("void %v()\n{\n", name)
		if hasVars {
			ctx.printf("\tlong res;")
		}
		if opts.Debug {
			// Use debug to avoid: error: ‘debug’ defined but not used.
			ctx.printf("\tdebug(\"%v\\n\");\n", name)
		}
		if opts.Repro {
			ctx.printf("\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		for _, c := range calls {
			ctx.printf("%s", c)
		}
		ctx.printf("}\n\n")
	} else {
		ctx.printf("void execute_call(int call)\n{\n")
		if hasVars {
			ctx.printf("\tlong res;")
		}
		ctx.printf("\tswitch (call) {\n")
		for i, c := range calls {
			ctx.printf("\tcase %v:\n", i)
			ctx.printf("%s", strings.Replace(c, "\t", "\t\t", -1))
			ctx.printf("\t\tbreak;\n")
		}
		ctx.printf("\t}\n")
		ctx.printf("}\n\n")

		ctx.printf("void %v()\n{\n", name)
		if opts.Debug {
			// Use debug to avoid: error: ‘debug’ defined but not used.
			ctx.printf("\tdebug(\"%v\\n\");\n", name)
		}
		if opts.Repro {
			ctx.printf("\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		ctx.printf("\texecute(%v);\n", len(calls))
		if opts.Collide {
			ctx.printf("\tcollide = 1;\n")
			ctx.printf("\texecute(%v);\n", len(calls))
		}
		ctx.printf("}\n\n")
	}
}

func (ctx *context) generateSyscallDefines() {
	prefix := ctx.sysTarget.SyscallPrefix
	for name, nr := range ctx.calls {
		if strings.HasPrefix(name, "syz_") || !ctx.sysTarget.NeedSyscallDefine(nr) {
			continue
		}
		ctx.printf("#ifndef %v%v\n", prefix, name)
		ctx.printf("#define %v%v %v\n", prefix, name, nr)
		ctx.printf("#endif\n")
	}
	if ctx.target.OS == "linux" && ctx.target.PtrSize == 4 {
		// This is a dirty hack.
		// On 32-bit linux mmap translated to old_mmap syscall which has a different signature.
		// mmap2 has the right signature. syz-extract translates mmap to mmap2, do the same here.
		ctx.printf("#undef __NR_mmap\n")
		ctx.printf("#define __NR_mmap __NR_mmap2\n")
	}
	ctx.printf("\n")
}

func (ctx *context) generateProgCalls(p *prog.Prog) ([]string, []uint64, error) {
	exec := make([]byte, prog.ExecBufferSize)
	progSize, err := p.SerializeForExec(exec)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize program: %v", err)
	}
	decoded, err := ctx.target.DeserializeExec(exec[:progSize])
	if err != nil {
		return nil, nil, err
	}
	calls, vars := ctx.generateCalls(decoded)
	return calls, vars, nil
}

func (ctx *context) generateCalls(p prog.ExecProg) ([]string, []uint64) {
	var calls []string
	csumSeq := 0
	for ci, call := range p.Calls {
		w := new(bytes.Buffer)
		// Copyin.
		for _, copyin := range call.Copyin {
			switch arg := copyin.Arg.(type) {
			case prog.ExecArgConst:
				if arg.BitfieldOffset == 0 && arg.BitfieldLength == 0 {
					fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = %v);\n",
						arg.Size*8, copyin.Addr, ctx.constArgToStr(arg))
				} else {
					fmt.Fprintf(w, "\tNONFAILING(STORE_BY_BITMASK(uint%v_t, 0x%x, %v, %v, %v));\n",
						arg.Size*8, copyin.Addr, ctx.constArgToStr(arg),
						arg.BitfieldOffset, arg.BitfieldLength)
				}
			case prog.ExecArgResult:
				fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = %v);\n",
					arg.Size*8, copyin.Addr, ctx.resultArgToStr(arg))
			case prog.ExecArgData:
				fmt.Fprintf(w, "\tNONFAILING(memcpy((void*)0x%x, \"%s\", %v));\n",
					copyin.Addr, toCString(arg.Data), len(arg.Data))
			case prog.ExecArgCsum:
				switch arg.Kind {
				case prog.ExecArgCsumInet:
					csumSeq++
					fmt.Fprintf(w, "\tstruct csum_inet csum_%d;\n", csumSeq)
					fmt.Fprintf(w, "\tcsum_inet_init(&csum_%d);\n", csumSeq)
					for i, chunk := range arg.Chunks {
						switch chunk.Kind {
						case prog.ExecArgCsumChunkData:
							fmt.Fprintf(w, "\tNONFAILING(csum_inet_update(&csum_%d, (const uint8_t*)0x%x, %d));\n", csumSeq, chunk.Value, chunk.Size)
						case prog.ExecArgCsumChunkConst:
							fmt.Fprintf(w, "\tuint%d_t csum_%d_chunk_%d = 0x%x;\n", chunk.Size*8, csumSeq, i, chunk.Value)
							fmt.Fprintf(w, "\tcsum_inet_update(&csum_%d, (const uint8_t*)&csum_%d_chunk_%d, %d);\n", csumSeq, csumSeq, i, chunk.Size)
						default:
							panic(fmt.Sprintf("unknown checksum chunk kind %v", chunk.Kind))
						}
					}
					fmt.Fprintf(w, "\tNONFAILING(*(uint16_t*)0x%x = csum_inet_digest(&csum_%d));\n", copyin.Addr, csumSeq)
				default:
					panic(fmt.Sprintf("unknown csum kind %v", arg.Kind))
				}
			default:
				panic(fmt.Sprintf("bad argument type: %+v", arg))
			}
		}

		// Call itself.
		if ctx.opts.Fault && ctx.opts.FaultCall == ci {
			fmt.Fprintf(w, "\twrite_file(\"/sys/kernel/debug/failslab/ignore-gfp-wait\", \"N\");\n")
			fmt.Fprintf(w, "\twrite_file(\"/sys/kernel/debug/fail_futex/ignore-private\", \"N\");\n")
			fmt.Fprintf(w, "\tinject_fault(%v);\n", ctx.opts.FaultNth)
		}
		callName := call.Meta.CallName
		resCopyout := call.Index != prog.ExecNoCopyout
		argCopyout := len(call.Copyout) != 0
		emitCall := ctx.opts.EnableTun || callName != "syz_emit_ethernet" &&
			callName != "syz_extract_tcp_res"
		// TODO: if we don't emit the call we must also not emit copyin, copyout and fault injection.
		// However, simply skipping whole iteration breaks tests due to unused static functions.
		if emitCall {
			native := !strings.HasPrefix(callName, "syz_")
			fmt.Fprintf(w, "\t")
			if resCopyout || argCopyout {
				fmt.Fprintf(w, "res = ")
			}
			if native {
				fmt.Fprintf(w, "syscall(%v%v", ctx.sysTarget.SyscallPrefix, callName)
			} else {
				fmt.Fprintf(w, "%v(", callName)
			}
			for ai, arg := range call.Args {
				if native || ai > 0 {
					fmt.Fprintf(w, ", ")
				}
				switch arg := arg.(type) {
				case prog.ExecArgConst:
					fmt.Fprintf(w, "%v", ctx.constArgToStr(arg))
				case prog.ExecArgResult:
					fmt.Fprintf(w, "%v", ctx.resultArgToStr(arg))
				default:
					panic(fmt.Sprintf("unknown arg type: %+v", arg))
				}
			}
			fmt.Fprintf(w, ");\n")
		}

		// Copyout.
		if resCopyout || argCopyout {
			fmt.Fprintf(w, "\tif (res != -1)")
			copyoutMultiple := len(call.Copyout) > 1 || resCopyout && len(call.Copyout) > 0
			if copyoutMultiple {
				fmt.Fprintf(w, " {")
			}
			fmt.Fprintf(w, "\n")
			if resCopyout {
				fmt.Fprintf(w, "\t\tr[%v] = res;\n", call.Index)
			}
			for _, copyout := range call.Copyout {
				fmt.Fprintf(w, "\t\tNONFAILING(r[%v] = *(uint%v_t*)0x%x);\n",
					copyout.Index, copyout.Size*8, copyout.Addr)
			}
			if copyoutMultiple {
				fmt.Fprintf(w, "\t}\n")
			}
		}
		calls = append(calls, w.String())
	}
	return calls, p.Vars
}

func (ctx *context) constArgToStr(arg prog.ExecArgConst) string {
	mask := (uint64(1) << (arg.Size * 8)) - 1
	v := arg.Value & mask
	val := fmt.Sprintf("%v", v)
	if v == ^uint64(0)&mask {
		val = "-1"
	} else if v >= 10 {
		val = fmt.Sprintf("0x%x", v)
	}
	if ctx.opts.Procs > 1 && arg.PidStride != 0 {
		val += fmt.Sprintf(" + procid*%v", arg.PidStride)
	}
	if arg.BigEndian {
		val = fmt.Sprintf("htobe%v(%v)", arg.Size*8, val)
	}
	return val
}

func (ctx *context) resultArgToStr(arg prog.ExecArgResult) string {
	res := fmt.Sprintf("r[%v]", arg.Index)
	if arg.DivOp != 0 {
		res = fmt.Sprintf("%v/%v", res, arg.DivOp)
	}
	if arg.AddOp != 0 {
		res = fmt.Sprintf("%v+%v", res, arg.AddOp)
	}
	return res
}

func toCString(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	readable := true
	for i, v := range data {
		// Allow 0 only as last byte.
		if !isReadable(v) && (i != len(data)-1 || v != 0) {
			readable = false
			break
		}
	}
	if !readable {
		buf := new(bytes.Buffer)
		for _, v := range data {
			buf.Write([]byte{'\\', 'x', toHex(v >> 4), toHex(v << 4 >> 4)})
		}
		return buf.Bytes()
	}
	if data[len(data)-1] == 0 {
		// Don't serialize last 0, C strings are 0-terminated anyway.
		data = data[:len(data)-1]
	}
	buf := new(bytes.Buffer)
	for _, v := range data {
		switch v {
		case '\t':
			buf.Write([]byte{'\\', 't'})
		case '\r':
			buf.Write([]byte{'\\', 'r'})
		case '\n':
			buf.Write([]byte{'\\', 'n'})
		case '\\':
			buf.Write([]byte{'\\', '\\'})
		case '"':
			buf.Write([]byte{'\\', '"'})
		default:
			if v < 0x20 || v >= 0x7f {
				panic("unexpected char during data serialization")
			}
			buf.WriteByte(v)
		}
	}
	return buf.Bytes()
}

func isReadable(v byte) bool {
	return v >= 0x20 && v < 0x7f || v == '\t' || v == '\r' || v == '\n'
}

func toHex(v byte) byte {
	if v < 10 {
		return '0' + v
	}
	return 'a' + v - 10
}

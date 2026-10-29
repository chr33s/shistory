"""Real interactive PTY tests, including complete Bash command-line capture."""

import errno
import fcntl
import struct
import termios
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
PROMPT = b"__SHISTORY_PROMPT__ "


class Shell:
    def __init__(self, executable, home, startup, extra_env=None):
        self.home = home
        self.db = home / "shistory" / "history.db"
        rc = home / (".zshrc" if "zsh" in executable else "bashrc")
        rc.write_text(startup)
        env = os.environ.copy()
        env.update(HOME=str(home), ZDOTDIR=str(home), SHISTORY_DB=str(self.db),
                   SHISTORY_CONFIG="", SHISTORY_IGNORE="", XDG_CONFIG_HOME=str(home / "config"),
                   PATH=str(ROOT / "bin") + os.pathsep + env["PATH"], TERM="xterm-256color")
        env.update(extra_env or {})
        argv = [executable, "-d", "-i"] if "zsh" in executable else [executable, "--noprofile", "--rcfile", str(rc), "-i"]
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.chdir(home)
            os.execvpe(executable, argv, env)
        self.pending = b""
        self.closed = False
        self.eof = False
        self.exit_status = None
        try:
            self.startup_output = self.prompt()
        except BaseException:
            self.close()
            raise

    def _read_output(self, timeout=0, capture=True):
        if self.fd is None or self.eof:
            return False
        if not select.select([self.fd], [], [], timeout)[0]:
            return False
        try:
            chunk = os.read(self.fd, 65536)
        except OSError as error:
            # Linux PTY masters report EIO when their last slave closes;
            # macOS reports an empty read. Both mean the output stream ended.
            if error.errno != errno.EIO:
                raise
            chunk = b""
        if not chunk:
            self.eof = True
            return False
        if capture:
            self.pending += chunk
        return True

    def prompt(self):
        deadline = time.monotonic() + 15
        while PROMPT not in self.pending:
            if time.monotonic() > deadline:
                raise AssertionError(f"prompt timed out: {self.pending.decode(errors='replace')}")
            self._read_output(0.1)
            if self.eof:
                raise AssertionError(f"shell ended: {self.pending!r}")
        output, self.pending = self.pending.split(PROMPT, 1)
        return output.decode(errors="replace")

    def command(self, text):
        os.write(self.fd, text.encode() + b"\n")
        return self.prompt()

    def drain(self):
        # Yield even if a child produces output continuously.
        for _ in range(64):
            if not self._read_output():
                break
        return self.pending.decode(errors="replace")

    def rows(self):
        with sqlite3.connect(self.db) as connection:
            connection.row_factory = sqlite3.Row
            return [dict(row) for row in connection.execute("SELECT * FROM history ORDER BY id")]

    def close(self):
        if self.closed:
            return
        self.closed = True
        try:
            # A loaded host can lose input written while the shell is still
            # handling SIGINT, so re-request exit whenever the shell goes quiet.
            deadline = time.monotonic() + 3
            while not self.eof:
                try:
                    os.write(self.fd, b"\x03")
                    time.sleep(0.05)
                    os.write(self.fd, b"exit 0\n")
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                exited = self._wait_for_exit(max(0, deadline - time.monotonic()), idle=0.5)
                if exited:
                    return
                if exited is False:
                    break
            if self._wait_for_exit(max(0, deadline - time.monotonic())):
                return
            # Darwin can wait for unread terminal output even after SIGKILL.
            # Disconnect the master before escalating, then poll without an
            # unbounded waitpid so a kernel/child failure cannot hang the suite.
            os.close(self.fd)
            self.fd = None
            self.eof = True
            try:
                os.kill(self.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            if not self._wait_for_exit(3):
                raise AssertionError(f"shell {self.pid} did not exit after PTY disconnect and SIGKILL")
        finally:
            if self.fd is not None:
                os.close(self.fd)
                self.fd = None

    def _wait_for_exit(self, timeout, idle=None):
        """True once reaped, False on timeout, None after `idle` quiet seconds."""
        deadline = time.monotonic() + timeout
        quiet_since = time.monotonic()
        while time.monotonic() < deadline:
            try:
                pid, status = os.waitpid(self.pid, os.WNOHANG)
            except ChildProcessError:
                return True
            if pid:
                self.exit_status = status
                return True
            if self.fd is None or self.eof:
                time.sleep(0.02)
            else:
                # Reading, rather than just waiting, lets terminal writes and
                # the OS's exit-time output drain finish. Discard cleanup output.
                if self._read_output(min(0.02, max(0, deadline - time.monotonic())), capture=False):
                    quiet_since = time.monotonic()
                elif idle is not None and time.monotonic() - quiet_since >= idle:
                    return None
        return False


class PTYLifecycleTests(unittest.TestCase):
    def test_cleanup_drains_exit_output_and_reaps_child(self):
        with tempfile.TemporaryDirectory(prefix="shistory-pty-") as temp:
            home = Path(temp).resolve()
            # More than the PTY buffer can hold: EXIT must finish writing before
            # it can create the marker and exit gracefully.
            startup = f"""PS1='{PROMPT.decode()}'
trap 'printf "%131072s" ""; printf complete > "$HOME/exit-complete"' EXIT
"""
            shell = Shell(shutil.which("bash"), home, startup)
            shell.close()
            shell.close()  # Cleanup is idempotent.
            self.assertEqual((home / "exit-complete").read_text(), "complete")
            self.assertEqual(os.waitstatus_to_exitcode(shell.exit_status), 0)
            self.assertIsNone(shell.fd)
            with self.assertRaises(ChildProcessError):
                os.waitpid(shell.pid, os.WNOHANG)

    def test_eof_does_not_spin_in_drain(self):
        with tempfile.TemporaryDirectory(prefix="shistory-pty-") as temp:
            shell = Shell(shutil.which("bash"), Path(temp).resolve(), f"PS1='{PROMPT.decode()}'\n")
            try:
                with self.assertRaisesRegex(AssertionError, "shell ended"):
                    shell.command("exit 0")
                self.assertTrue(shell.eof)
                shell.drain()
            finally:
                shell.close()
            self.assertEqual(os.waitstatus_to_exitcode(shell.exit_status), 0)

    def test_cleanup_retries_lost_exit_request(self):
        with tempfile.TemporaryDirectory(prefix="shistory-pty-") as temp:
            home = Path(temp).resolve()
            # Simulates `exit 0` input lost while the shell handles SIGINT.
            startup = f"""PS1='{PROMPT.decode()}'
trap 'printf complete > "$HOME/exit-complete"' EXIT
function exit {{ if [[ -z $_lost ]]; then _lost=1; return; fi; builtin exit "$@"; }}
"""
            shell = Shell(shutil.which("bash"), home, startup)
            shell.close()
            self.assertEqual((home / "exit-complete").read_text(), "complete")
            self.assertEqual(os.waitstatus_to_exitcode(shell.exit_status), 0)

    def test_cleanup_kills_shell_that_ignores_exit_and_hangup(self):
        with tempfile.TemporaryDirectory(prefix="shistory-pty-") as temp:
            startup = f"""PS1='{PROMPT.decode()}'
trap '' HUP INT TERM
function exit {{ :; }}
"""
            shell = Shell(shutil.which("bash"), Path(temp).resolve(), startup)
            started = time.monotonic()
            shell.close()
            self.assertLess(time.monotonic() - started, 7)
            self.assertEqual(os.waitstatus_to_exitcode(shell.exit_status), -signal.SIGKILL)
            self.assertIsNone(shell.fd)
            with self.assertRaises(ChildProcessError):
                os.waitpid(shell.pid, os.WNOHANG)

    def test_startup_failure_closes_pty_and_reaps_child(self):
        with tempfile.TemporaryDirectory(prefix="shistory-pty-") as temp:
            shell = Shell.__new__(Shell)
            with self.assertRaisesRegex(AssertionError, "shell ended"):
                shell.__init__(shutil.which("bash"), Path(temp).resolve(), "exit 7\n")
            self.assertTrue(shell.closed)
            self.assertIsNone(shell.fd)
            self.assertEqual(os.waitstatus_to_exitcode(shell.exit_status), 7)
            with self.assertRaises(ChildProcessError):
                os.waitpid(shell.pid, os.WNOHANG)


class ShellTests(unittest.TestCase):
    def run_shell(self, executable, array=False):
        with tempfile.TemporaryDirectory(prefix="shistory-shell-") as temp:
            home = Path(temp).resolve()
            for name in ("project/api", "project/web", "other", "outside"):
                (home / name).mkdir(parents=True)
            for name in ("project", "other"):
                subprocess.run(["git", "init", "-q", str(home / name)], check=True)
            is_zsh = "zsh" in executable
            # Apple Bash 3.2 history -a does not create a missing HISTFILE.
            (home / "native_history").touch(mode=0o600)
            if is_zsh:
                startup = f"""PS1='{PROMPT.decode()}'
HISTFILE=$HOME/native_history
HISTSIZE=10000
SAVEHIST=10000
setopt incappendhistory
autoload -Uz add-zsh-hook
_existing_preexec() {{ :; }}
_existing_precmd() {{ :; }}
_existing_chpwd() {{ :; }}
add-zsh-hook preexec _existing_preexec
add-zsh-hook precmd _existing_precmd
add-zsh-hook chpwd _existing_chpwd
alias hg='echo original-hg'
source '{ROOT}/shistory.plugin.zsh'
source '{ROOT}/shistory.plugin.zsh'
"""
            else:
                prompt_command = "PROMPT_COMMAND=(_existing_prompt ':')" if array else "PROMPT_COMMAND='_existing_prompt; :'"
                startup = f"""PS1='{PROMPT.decode()}'
PS2='__CONTINUATION__ '
HISTFILE=$HOME/native_history
HISTSIZE=10000
HISTCONTROL=
HISTIGNORE=
shopt -s cmdhist lithist
_existing_prompt() {{ _existing_status=$?; printf '%s\\n' "$_existing_status" >> "$HOME/prompt-status"; }}
trap '_existing_debug=1' DEBUG
alias hg='echo original-hg'
{prompt_command}
source '{ROOT}/shell/shistory.bash'
source '{ROOT}/shell/shistory.bash'
"""
            shell = Shell(executable, home, startup)
            try:
                shell.command("printf '%s\\n' first")
                shell.command("false")
                shell.command("cd project/api")
                shell.command("printf '%s\\n' api | cat; false")
                shell.command("cd ../web")
                shell.command("printf '%s\\n' web")
                shell.command("cd ../../other")
                shell.command("printf '%s\\n' other")
                shell.command("cd ../outside")
                shell.command("printf '%s\\n' outside")
                shell.command(" printf secret")
                shell.command("printf '%s\\n' 'multi\nline'")
                rows = shell.rows()
                commands = [row["command"] for row in rows]
                self.assertNotIn(" printf secret", commands)
                self.assertEqual(len(rows), 11, commands)
                self.assertEqual(rows[1]["exit_status"], 1)
                self.assertEqual(rows[3]["exit_status"], 1)
                self.assertEqual(rows[2]["cwd"], str(home))
                self.assertEqual(rows[3]["scope_path"], str(home / "project"))
                self.assertEqual(rows[4]["cwd"], str(home / "project/api"))
                self.assertEqual(rows[5]["scope_path"], str(home / "project"))
                self.assertEqual(rows[7]["scope_path"], str(home / "other"))
                self.assertEqual(rows[9]["scope_type"], "directory")
                self.assertEqual(rows[-1]["command"], "printf '%s\\n' 'multi\nline'")
                self.assertTrue(all(row["duration_ms"] >= 0 for row in rows))
                self.assertTrue(all(abs(row["started_at"] - int(time.time() * 1000)) < 60000 for row in rows))
                self.assertEqual(len({row["session_id"] for row in rows}), 1)
                if is_zsh:
                    result = shell.command("print -r -- $preexec_functions $precmd_functions $chpwd_functions")
                    for hook in ("_existing_preexec", "_existing_precmd", "_existing_chpwd"):
                        self.assertIn(hook, result)
                    self.assertEqual(result.count("_shistory_preexec"), 1, result)
                    shell.command("BUFFER='printf'; _zsh_autosuggest_strategy_shistory 'printf'; print -r -- SUGGEST:$suggestion")
                    result = shell.command("print -r -- SCOPE:$SHISTORY_SCOPE")
                    self.assertIn("SCOPE:" + str(home / "outside"), result)
                else:
                    statuses = (home / "prompt-status").read_text().splitlines()
                    self.assertIn("1", statuses)
                    self.assertIn("DEBUG:1", shell.command("printf 'DEBUG:%s\\n' \"$_existing_debug\""))
                self.assertTrue((home / "native_history").exists())
                self.assertIn("original-hg", shell.command("hg"))
                # Shell re-exec creates a new session, still only one set of hooks.
                first_session = rows[0]["session_id"]
                if is_zsh:
                    shell.command("exec zsh -d -i")
                else:
                    shell.command(f"exec '{executable}' --noprofile --rcfile '{home}/bashrc' -i")
                shell.command("printf reexec")
                self.assertNotEqual(shell.rows()[-1]["session_id"], first_session)
            finally:
                shell.close()

    def test_zsh(self):
        self.run_shell(shutil.which("zsh"))

    def test_bash_scalar(self):
        self.run_shell(shutil.which("bash"))

    def test_bash_array(self):
        self.run_shell(shutil.which("bash"), array=True)

    def test_macos_bash32(self):
        if not Path("/bin/bash").exists() or "3.2" not in subprocess.check_output(["/bin/bash", "--version"], text=True):
            self.skipTest("system Bash 3.2 not available")
        self.run_shell("/bin/bash")

    def run_selector(self, executable, live=False):
        with tempfile.TemporaryDirectory(prefix="shistory-selector-") as temp:
            home = Path(temp).resolve()
            selected = "printf 'selector-marker-é\\n'\n# tab\tand trailing newline\n"
            if live:
                selected += "# '$(touch INJECTED)'\n"
                config = home / "config/shistory"
                config.mkdir(parents=True)
                (config / "config.toml").write_text("[history]\nmax_results = 1\n")
            env = dict(os.environ, HOME=str(home), SHISTORY_DB=str(home / "shistory/history.db"), SHISTORY_CONFIG="", XDG_CONFIG_HOME=str(home / "config"), SHISTORY_IGNORE="")
            subprocess.run([str(ROOT / "bin/shistory"), "record", "--session", "selector", "--seq", "1", "--shell", "test", "--cwd", str(home), "--command", selected], env=env, check=True)
            if live:
                for seq in (2, 3):
                    subprocess.run([str(ROOT / "bin/shistory"), "record", "--session", "selector", "--seq", str(seq), "--shell", "test", "--cwd", str(home), "--command", "echo recent-winner"], env=env, check=True)
            if "zsh" in executable:
                startup = f"""PS1='{PROMPT.decode()}'
bindkey -e
source '{ROOT}/shistory.plugin.zsh'
_selector_dump() {{ print -rn -- "$BUFFER" > "$HOME/buffer"; }}
_selector_clear() {{ BUFFER=''; }}
zle -N _selector_dump
zle -N _selector_clear
bindkey '^O' _selector_dump
bindkey '^W' _selector_clear
bindkey '^R' shistory-reverse-search
"""
            else:
                startup = f"""PS1='{PROMPT.decode()}'
source '{ROOT}/shell/shistory.bash'
_selector_dump() {{ printf '%s' "$READLINE_LINE" > "$HOME/buffer"; }}
_selector_clear() {{ READLINE_LINE=''; READLINE_POINT=0; }}
bind -x '"\\C-o":_selector_dump'
bind -x '"\\C-w":_selector_clear'
"""
            shell = Shell(executable, home, startup, {"FZF_DEFAULT_OPTS": "--height=12 --no-clear" if live else "--filter=selector-marker", "FZF_DEFAULT_OPTS_FILE": "", "SHISTORY_BIND_KEYS": "1"})
            try:
                # The prompt is printed just before the editor enters raw mode.
                time.sleep(0.15)
                def clear_editor():
                    os.write(shell.fd, b"\x17")
                    time.sleep(0.15)
                    cleared = shell.drain()
                    shell.pending = b""
                    os.write(shell.fd, b"\n")
                    try:
                        shell.prompt()
                    except AssertionError as error:
                        raise AssertionError(cleared) from error
                    time.sleep(0.15)
                if live:
                    fcntl.ioctl(shell.fd, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 100, 0, 0))
                os.write(shell.fd, b"\x12")
                if live:
                    deadline = time.monotonic() + 10
                    while "recent-winner" not in shell.drain():
                        if time.monotonic() > deadline:
                            self.fail("initial live results missing: " + shell.drain())
                        time.sleep(0.02)
                    self.assertNotIn("selector-marker", shell.drain())
                    shell.pending = b""
                    # Quoted fzf placeholders must keep query text out of shell evaluation.
                    os.write(shell.fd, b"'$(touch INJECTED)'")
                    deadline = time.monotonic() + 10
                    while "selector-marker" not in shell.drain():
                        if time.monotonic() > deadline:
                            self.fail("live reload did not find older command: " + shell.drain())
                        time.sleep(0.02)
                    os.write(shell.fd, b"\r")
                    shell.prompt()
                time.sleep(0.2)
                os.write(shell.fd, b"\x0f")
                deadline = time.monotonic() + 15
                path = home / "buffer"
                while not path.exists():
                    shell.drain()
                    if time.monotonic() > deadline:
                        self.fail("selector buffer capture timed out: " + shell.startup_output + shell.drain())
                    time.sleep(0.01)
                self.assertEqual(path.read_text(), selected)
                self.assertEqual(len(shell.rows()), 3 if live else 1, "selection must not execute")
                self.assertFalse((home / "INJECTED").exists(), "query must not execute shell syntax")
                clear_editor()
                if live:
                    return
                shell.close()
                shell = Shell(executable, home, startup, {"FZF_DEFAULT_OPTS": "--filter=no-match-selector", "FZF_DEFAULT_OPTS_FILE": "", "SHISTORY_BIND_KEYS": "1"})
                time.sleep(0.15)
                path.unlink()
                os.write(shell.fd, b"unsubmitted\x12")
                time.sleep(0.2)
                os.write(shell.fd, b"\x0f")
                deadline = time.monotonic() + 3
                while not path.exists():
                    shell.drain()
                    if time.monotonic() > deadline:
                        self.fail("cancelled selector buffer capture timed out: " + shell.drain())
                    time.sleep(0.01)
                self.assertEqual(path.read_text(), "unsubmitted")
                clear_editor()
            finally:
                shell.close()

    def test_zsh_live_selector(self):
        self.run_selector(shutil.which("zsh"), live=True)

    def test_bash_live_selector(self):
        if int(subprocess.check_output([shutil.which("bash"), "-c", "echo ${BASH_VERSINFO[0]}"], text=True)) < 4:
            self.skipTest("Readline bind -x requires Bash 4+")
        self.run_selector(shutil.which("bash"), live=True)

    def test_zsh_selector(self):
        self.run_selector(shutil.which("zsh"))

    def test_bash_selector(self):
        if int(subprocess.check_output([shutil.which("bash"), "-c", "echo ${BASH_VERSINFO[0]}"], text=True)) < 4:
            self.skipTest("Readline bind -x requires Bash 4+")
        self.run_selector(shutil.which("bash"))


if __name__ == "__main__":
    unittest.main(verbosity=2)

import paramiko
import sys

c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
c.connect("107.150.62.202", port=4413, username="root", password="YHyZQMz564N2Y268", timeout=30)
s = c.open_sftp()
s.put(r"D:\btc\cache_test\cnt-src-0460.tgz", "/root/rr-cnt/src.tgz")
s.close()
cmd = (
    "cd /root/rr-cnt && rm -rf src && mkdir src && tar xzf src.tgz -C src --strip-components=1 && "
    "ls src && grep -n '^version' src/Cargo.toml && ls -l src/src/resistor.rs && echo '--- pull ---' && tail -3 pull.log"
)
i, o, e = c.exec_command(cmd, timeout=180)
sys.stdout.write(o.read().decode("utf-8", "replace"))
sys.stderr.write(e.read().decode("utf-8", "replace"))
c.close()

"""在测试机 154 上跑命令的小工具。

用法：
  python rr.py "远端命令"              # 执行命令
  python rr.py -f local_script.sh       # 把本地脚本传上去再执行
环境变量：RR_HOST / RR_PORT / RR_USER / RR_PASS 可覆盖默认值。
"""
import os
import sys
import paramiko

HOST = os.environ.get("RR_HOST", "154.217.250.100")
PORT = int(os.environ.get("RR_PORT", "3337"))
USER = os.environ.get("RR_USER", "root")
PASS = os.environ.get("RR_PASS", "EA9exEMWBaYAB4d8")


def main():
    if len(sys.argv) < 2:
        sys.exit("usage: rr.py [-f script] | 'command'")
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(HOST, port=PORT, username=USER, password=PASS,
                   timeout=25, banner_timeout=25, auth_timeout=25)
    if sys.argv[1] == "-f":
        local = sys.argv[2]
        remote = "/tmp/rr_" + os.path.basename(local)
        sftp = client.open_sftp()
        sftp.put(local, remote)
        sftp.close()
        command = "bash " + remote
    else:
        command = sys.argv[1]
    stdin, stdout, stderr = client.exec_command(command, timeout=int(os.environ.get("RR_TIMEOUT", "1800")))
    out = stdout.read().decode("utf-8", "replace")
    err = stderr.read().decode("utf-8", "replace")
    code = stdout.channel.recv_exit_status()
    sys.stdout.write(out)
    if err:
        sys.stderr.write(err)
    client.close()
    sys.exit(code)


if __name__ == "__main__":
    main()

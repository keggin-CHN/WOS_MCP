import paramiko
import os
import sys

host = '172.207.250.61'
port = 22
username = 'keggin'
password = 'YOUR_PASSWORD_HERE'

local_base = r'c:\code\wos'
remote_base = '/home/keggin/wos_mcp_server'

ssh = paramiko.SSHClient()
ssh.set_missing_host_key_policy(paramiko.AutoAddPolicy())

try:
    print(f"Connecting to {host}...")
    ssh.connect(host, port, username, password, timeout=10)
    sftp = ssh.open_sftp()
    
    # Create remote directory
    try:
        sftp.mkdir(remote_base)
    except Exception:
        pass
    try:
        sftp.mkdir(remote_base + '/wos_mcp')
    except Exception:
        pass

    files_to_upload = [
        ('config.json', 'config.json'),
        ('deploy_remote.sh', 'deploy_remote.sh'),
    ]
    
    # Get all .py and .txt files in wos_mcp
    for f in os.listdir(os.path.join(local_base, 'wos_mcp')):
        if f.endswith('.py') or f.endswith('.txt') or f.endswith('.json'):
            files_to_upload.append((f'wos_mcp/{f}', f'wos_mcp/{f}'))
            
    print(f"Uploading {len(files_to_upload)} files...")
    for local_path, remote_path in files_to_upload:
        try:
            sftp.put(os.path.join(local_base, local_path), remote_base + '/' + remote_path)
        except Exception as e:
            print(f"Failed to upload {local_path}: {e}")
            
    # chmod +x deploy_remote.sh
    sftp.chmod(remote_base + '/deploy_remote.sh', 0o755)
    sftp.close()
    
    # Run deploy script
    print("Executing deploy_remote.sh...")
    stdin, stdout, stderr = ssh.exec_command(f'cd {remote_base} && dos2unix deploy_remote.sh 2>/dev/null || true && ./deploy_remote.sh')
    
    # wait for script to finish
    exit_status = stdout.channel.recv_exit_status()
    print(stdout.read().decode('utf-8'))
    print(stderr.read().decode('utf-8'))
    print(f"Deployment script exited with status: {exit_status}")
    
except Exception as e:
    print(f"Error: {e}")
finally:
    ssh.close()

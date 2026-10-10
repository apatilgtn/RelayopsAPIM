param(
  [string]$Profile = 'relayops',
  [string]$StackName = 'relayops-sydney',
  [string]$ConsoleDomain,
  [string]$ApiDomain
)
# Run locally after AWS review/access is available. Does not create another server.
$ErrorActionPreference = 'Stop'
$taskRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$taskAws = Join-Path $taskRoot 'bin/aws-cli-portable/Amazon/AWSCLIV2/aws.exe'
if (!(Test-Path -LiteralPath $taskAws)) { throw 'Workspace AWS CLI is missing' }
function Invoke-DeploymentAws {
  param([string[]]$AwsArguments)
  $taskResult = & $taskAws @AwsArguments --profile $Profile --region ap-southeast-2 --no-cli-pager
  if ($LASTEXITCODE -ne 0) { throw 'AWS operation failed; deployment stopped' }
  return $taskResult
}
$taskIdentity = Invoke-DeploymentAws -AwsArguments @('sts','get-caller-identity','--output','json') | ConvertFrom-Json
if ($taskIdentity.Account -ne '282583951405') { throw 'Wrong AWS account; stopped' }
$taskStack = Invoke-DeploymentAws -AwsArguments @('cloudformation','describe-stacks','--stack-name',$StackName,'--output','json') | ConvertFrom-Json
if ($taskStack.Stacks[0].StackStatus -ne 'CREATE_COMPLETE') { throw "Stack is $($taskStack.Stacks[0].StackStatus); inspect its events before continuing" }
$taskOutputs = @{}
foreach ($taskOutput in $taskStack.Stacks[0].Outputs) { $taskOutputs[$taskOutput.OutputKey] = $taskOutput.OutputValue }
if (!$ConsoleDomain) { $ConsoleDomain = "console.$($taskOutputs.PublicIp).sslip.io" }
if (!$ApiDomain) { $ApiDomain = "api.$($taskOutputs.PublicIp).sslip.io" }
foreach ($taskDomain in @($ConsoleDomain,$ApiDomain)) {
  if ($taskDomain -notmatch '^[a-zA-Z0-9.-]+$') { throw 'Invalid domain name' }
}
$taskInstances = Invoke-DeploymentAws -AwsArguments @('ssm','describe-instance-information','--output','json') | ConvertFrom-Json
if (!($taskInstances.InstanceInformationList | Where-Object { $_.InstanceId -eq $taskOutputs.InstanceId -and $_.PingStatus -eq 'Online' })) { throw 'Server is not online in SSM yet' }
$taskArchive = Join-Path $taskRoot 'bin/relayops-release.tar.gz'
$taskChecksum = (Get-FileHash -LiteralPath $taskArchive -Algorithm SHA256).Hash.ToLowerInvariant()
Invoke-DeploymentAws -AwsArguments @('s3','cp',$taskArchive,"s3://$($taskOutputs.ArtifactBucket)/relayops-release.tar.gz",'--only-show-errors') | Out-Null
$taskInstaller = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'install-release.sh') -Raw
$taskRemoteCommand = "bash -s -- '$($taskOutputs.ArtifactBucket)' '$ConsoleDomain' '$ApiDomain' '$taskChecksum' <<'RELAYOPS_INSTALL_SCRIPT'`n" + $taskInstaller.Replace("`r`n","`n") + "`nRELAYOPS_INSTALL_SCRIPT"
$taskRequest = @{
  DocumentName = 'AWS-RunShellScript'
  InstanceIds = @($taskOutputs.InstanceId)
  TimeoutSeconds = 600
  Parameters = @{ commands = @($taskRemoteCommand); executionTimeout = @('600') }
}
$taskRequestFile = Join-Path $taskRoot 'bin/aws-install-request.json'
[IO.File]::WriteAllText($taskRequestFile, ($taskRequest | ConvertTo-Json -Depth 8), [Text.UTF8Encoding]::new($false))
try {
  $taskCommand = Invoke-DeploymentAws -AwsArguments @('ssm','send-command','--cli-input-json',('file://' + $taskRequestFile),'--output','json') | ConvertFrom-Json
} finally { Remove-Item -LiteralPath $taskRequestFile -ErrorAction SilentlyContinue }
$taskReceipt = @{ InstanceId=$taskOutputs.InstanceId; CommandId=$taskCommand.Command.CommandId; ConsoleUrl="https://$ConsoleDomain"; ApiUrl="https://$ApiDomain"; ArtifactBucket=$taskOutputs.ArtifactBucket }
$taskReceipt | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $taskRoot 'bin/aws-deployment-receipt.json')
Write-Output "Installation requested. Command ID: $($taskCommand.Command.CommandId)"
Write-Output "Intended console URL (verify before use): https://$ConsoleDomain"
Write-Output 'Inspect the SSM command result, HTTPS and database readiness before declaring deployment complete.'

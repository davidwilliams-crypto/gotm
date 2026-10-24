# Signing gotm with Azure Trusted Signing

Azure Trusted Signing is Microsoft's code-signing service: Microsoft validates
who you are once, then issues short-lived certificates whenever you sign.
Microsoft is renaming it **Artifact Signing**; the portal and its quickstart
may show either name, the resources, roles and endpoints are the same.
Signed builds chain to the "Microsoft Identity Verification Root Certificate
Authority 2020", Windows shows your organisation as the publisher, and
SmartScreen reputation accrues to that identity across releases.

The build PC side is done: `build.ps1 -TrustedSigning` signs both executables
with Microsoft's signtool plug-in (`tools\get-signing-client.ps1` fetches the
`Microsoft.Trusted.Signing.Client` package from NuGet once into `tools\signing\`
and checks its DLL for a valid Microsoft Authenticode signature on every use),
and `signing.json` holds the (non-secret) account settings. The Azure side
needs an account owner, so it is a one-time job for you. Steps 1–6 are in the
Azure portal; 7–9 are on the build PC.

## 1. Subscription

Sign in to the [Azure portal](https://portal.azure.com) with an account that
can create resources in a subscription. If the company has no subscription,
create one (pay-as-you-go). Trusted Signing's Basic tier is billed monthly
(about USD 10 at the time of writing; Microsoft's product page currently says
the service is free with consumption pricing "coming soon").

## 2. Register the resource provider

Subscription ▸ Resource providers ▸ search `Microsoft.CodeSigning` ▸ Register.
Takes a minute.

## 3. Create the Trusted Signing account

Search "Trusted Signing Accounts" ▸ Create.

| Field | Value |
|---|---|
| Resource group | new, e.g. `rg-signing` |
| Account name | e.g. `yourcompany-signing` (becomes part of the endpoint, 3–24 lowercase letters/digits) |
| Region | **East US** (nearest region to Canada; the account's region fixes the endpoint, e.g. `https://eus.codesigning.azure.net`) |
| Pricing tier | Basic |

After creation, the account's **Overview** page shows the *Account URI*
(the endpoint) and the account name. Copy both into `signing.json`.

## 4. Identity validation (the part that takes days)

Trusted Signing account ▸ Identity validations ▸ New identity ▸
**Organization** (public trust).

- Legal business name, registered address, primary email and website, exactly
  as they appear in public records (for a Canadian company: the federal or
  provincial corporate registry). Microsoft checks those records; a mismatch
  is the usual cause of a rejection.
- The organisation must have at least three years of verifiable history.
- The email contact receives a verification message; answer it.
- Status moves from *In progress* to *Completed*; allow one to seven business
  days. Microsoft may ask for documents (articles of incorporation, a utility
  bill, a notarised letter) through the portal.

The validated legal name is what Windows will show as the publisher.

## 5. Certificate profile

Trusted Signing account ▸ Certificate profiles ▸ Create.

| Field | Value |
|---|---|
| Profile type | **Public Trust** |
| Name | e.g. `gotm` (copy it into `signing.json` as `profile`) |
| Verified CN and O | pick the completed identity validation |

The profile is ready as soon as the validation is complete.

## 6. Permission to sign

Trusted Signing account ▸ Access control (IAM) ▸ Add role assignment ▸ role
**Trusted Signing Certificate Profile Signer** ▸ assign it to the user who
runs the builds (or to a service principal for CI).

Without this role the build fails with a 403 from the signing endpoint.

## 7. Sign in on the build PC

The signing client finds credentials the way Azure SDKs do, in this order:
environment variables (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`,
`AZURE_CLIENT_SECRET` for a service principal), Azure CLI, Azure PowerShell.
For interactive use, install either and sign in once:

```powershell
winget install Microsoft.AzureCLI
az login --scope "https://codesigning.azure.net/.default"
```

or

```powershell
Install-Module Az.Accounts -Scope CurrentUser
Connect-AzAccount
```

Sign in with the same user that received the role in step 6.

## 8. Fill in signing.json

```json
{
  "endpoint": "https://eus.codesigning.azure.net",
  "account":  "yourcompany-signing",
  "profile":  "gotm"
}
```

## 9. Build signed

```powershell
.\build.ps1 -Version 0.9.2 -TrustedSigning
```

The script signs `gotm.exe` and the installer, timestamps them against
Microsoft's RFC 3161 server (so the signatures outlive the short-lived
certificate), and then runs `signtool verify /pa` on each file; a build with
an invalid signature fails. Check a build with:

```powershell
signtool verify /pa /v dist\gotm.exe
```

The chain should end in "Microsoft Identity Verification Root Certificate
Authority 2020" and the signer should be your organisation.

## What to expect from SmartScreen

A signature removes the "Unknown publisher" line and ties reputation to your
organisation rather than to each file. Reputation itself still accrues from
downloads and installs of signed builds; the first users may still see the
warning for a while. Nothing more can be done about that from the build side.

## Troubleshooting

- "downloading Microsoft.Trusted.Signing.Client" fails: the first signed build
  needs internet access to nuget.org; afterwards the client is cached in
  `tools\signing\`. The plug-in needs the .NET 8 runtime (present on this PC).
- "is not validly signed": the cached client was altered or the download was
  corrupted; delete `tools\signing\` and build again.
- "No credential available" / a browser does not open: sign in (step 7) in
  the same console session that runs the build, or set the three `AZURE_*`
  variables. `signing.json` may list `"excludeCredentials"` to skip credential
  sources (by default the cloud-only managed-identity and workload-identity
  sources are skipped so a build PC does not wait on them).
- 403 Forbidden: the signed-in user lacks the Certificate Profile Signer role
  (step 6), or the profile name or account name in `signing.json` is wrong.
- "Identity validation not completed": step 4 is still in progress; the
  profile cannot issue certificates until it is.

import { api, ApiError, type Config, type DocType, type DocumentMeta } from './api'
import { formatBytes } from './format'

// checkFile applies the server's limits before anything is sent, so a user
// gets an instant message instead of a round trip. It is only a courtesy:
// the server checks the declared type and size again, then sniffs the
// stored bytes, so a renamed .exe is still rejected.
export function checkFile(cfg: Config, file: { type: string; size: number }): string | null {
  if (!cfg.allowed_types.includes(file.type)) {
    return 'Only PDF, PNG or JPEG files are accepted.'
  }
  if (file.size === 0) {
    return 'The file is empty.'
  }
  const limit = file.type.startsWith('image/') ? cfg.max_image_bytes : cfg.max_upload_bytes
  if (file.size > limit) {
    return `The file is ${formatBytes(file.size)}; the limit for this type is ${formatBytes(limit)}.`
  }
  return null
}

// uploadFile runs the three-step upload that works the same on AWS and
// locally:
//  1. ask the API for a short-lived upload URL (it records the document as
//     pending_upload),
//  2. PUT the bytes straight to that URL (S3 on AWS, so file bytes never pass
//     through the Lambda),
//  3. tell the API the upload is complete so it can check size, type and
//     SHA-256 of what actually arrived.
export async function uploadFile(
  cfg: Config,
  shipmentId: string,
  type: DocType,
  file: File,
  onStep: (step: string) => void = () => {},
): Promise<DocumentMeta> {
  const problem = checkFile(cfg, file)
  if (problem) throw new ApiError(0, 'file_rejected', problem)

  onStep('Requesting upload link…')
  const { document, upload } = await api.createUpload(shipmentId, {
    type,
    filename: file.name,
    content_type: file.type,
    size_bytes: file.size,
  })

  onStep('Sending file…')
  let res: Response
  try {
    // Headers are part of the signature, so they are sent exactly as given.
    res = await fetch(upload.url, { method: upload.method, headers: upload.headers, body: file })
  } catch {
    throw new ApiError(
      0,
      'storage_unreachable',
      'The file could not be sent to storage. Check the connection and try again.',
    )
  }
  if (!res.ok) {
    // S3 answers with XML, not our JSON shape; the status code is enough.
    throw new ApiError(
      res.status,
      'storage_upload_failed',
      `Storage refused the file (HTTP ${res.status}). The upload link may have expired; please try again.`,
    )
  }

  onStep('Checking the stored file…')
  return api.completeUpload(document.id)
}

// fetchSample downloads one of the bundled synthetic PDFs so the sample
// buttons go through exactly the same upload flow as a user's file.
export async function fetchSample(name: string): Promise<File> {
  const res = await fetch(`/samples/${name}`)
  const blob = res.ok ? await res.blob() : null
  // The Vite dev server answers unknown paths with index.html, so check the
  // type as well as the status.
  if (!blob || !blob.type.includes('pdf')) {
    throw new ApiError(res.status, 'sample_missing', `The sample file ${name} is not included in this build.`)
  }
  return new File([blob], name, { type: 'application/pdf' })
}

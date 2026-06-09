ALTER TABLE "Photo"
  ADD COLUMN IF NOT EXISTS "imageLabelRawResult" jsonb,
  ADD COLUMN IF NOT EXISTS "imageLabelSuggestions" jsonb,
  ADD COLUMN IF NOT EXISTS "imageLabelStatus" text,
  ADD COLUMN IF NOT EXISTS "imageLabelFailReason" text,
  ADD COLUMN IF NOT EXISTS "imageLabelUpdatedAt" timestamptz;

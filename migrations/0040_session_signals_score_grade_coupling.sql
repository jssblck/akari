-- score and grade are derived together and must stay consistent: quality.Score computes a
-- 0-100 score and sets grade = quality.GradeFor(score) in the same call, and refreshSignalsTx
-- writes them as one unit (both set for a scored session, both NULL for an unscored one). Every
-- read that defines or summarizes the graded cohort leans on that: the Insights Grades panel
-- groups sig.grade, the session page's Quality tile gates on SessionSignals.Scored() (both
-- non-NULL), and the project OG card averages sig.score and maps the mean through GradeFor to a
-- representative letter. Those views only reconcile if a graded row's score and grade agree; the
-- columns were independently nullable and free to disagree, so the agreement rested on the
-- writer's discipline alone.
--
-- Two CHECKs make it a database invariant, the same belt-and-suspenders the hygiene-count CHECK
-- uses (the deriving code preserves it by construction; the constraint makes a future bug fail
-- loudly on write rather than quietly skewing a grade or splitting a cohort):
--
--   1. score and grade are both set or both NULL, so "scored" is one predicate however a reader
--      spells it (grade IS NOT NULL, score IS NOT NULL, or Scored()).
--   2. a non-NULL grade equals GradeFor(score), so the card's letter-of-the-mean-score and the
--      panel's stored grades derive from one consistent score->grade mapping.
--
-- The band thresholds below mirror quality.GradeFor. They are scoring policy, so a change to the
-- GradeFor bands is a scoring change that already bumps quality.Version and rides an epoch
-- reparse; this constraint must move with it (and the reparse re-derives every row to satisfy
-- the new bands). Existing rows all satisfy both CHECKs (the only writer couples and bands them),
-- so the constraints validate without a backfill.
ALTER TABLE session_signals
  ADD CONSTRAINT session_signals_score_grade_ck
  CHECK ((score IS NULL) = (grade IS NULL));

ALTER TABLE session_signals
  ADD CONSTRAINT session_signals_grade_matches_score_ck
  CHECK (
    grade IS NULL OR grade = CASE
      WHEN score >= 90 THEN 'A'
      WHEN score >= 75 THEN 'B'
      WHEN score >= 60 THEN 'C'
      WHEN score >= 40 THEN 'D'
      ELSE 'F'
    END
  );

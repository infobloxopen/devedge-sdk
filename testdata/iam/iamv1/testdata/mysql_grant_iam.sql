-- F033 P2 test fixture: the testcontainers mysql module creates the `iam` user with
-- access only to the `iam` schema and leaves remote `root` denied. The harness creates
-- a fresh, uniquely named database per test, which needs CREATE DATABASE privilege, so
-- grant the `iam` user full privileges (this is a throwaway test container only).
GRANT ALL PRIVILEGES ON *.* TO 'iam'@'%' WITH GRANT OPTION;
FLUSH PRIVILEGES;

-- Test user: alice@test.com / password123
-- bcrypt hash of "password123" (cost 12)
INSERT INTO users (id, email, password_hash)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'alice@test.com',
    '$2a$12$LJ3m4ys3Lk0TSwMC/VAPquGpGjRHVJ.sI0VBs7BYxNMYxJ4GQqMi2'
) ON CONFLICT DO NOTHING;

-- Test user: bob@test.com / password123
INSERT INTO users (id, email, password_hash)
VALUES (
    '00000000-0000-0000-0000-000000000002',
    'bob@test.com',
    '$2a$12$LJ3m4ys3Lk0TSwMC/VAPquGpGjRHVJ.sI0VBs7BYxNMYxJ4GQqMi2'
) ON CONFLICT DO NOTHING;

-- Alice's root folders
INSERT INTO nodes (id, owner_id, parent_id, name, type) VALUES
    ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', NULL, 'Documents', 'folder'),
    ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000001', NULL, 'Photos', 'folder')
ON CONFLICT DO NOTHING;

-- Subfolder inside Documents
INSERT INTO nodes (id, owner_id, parent_id, name, type) VALUES
    ('00000000-0000-0000-0000-000000000012', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000010', 'Reports', 'folder')
ON CONFLICT DO NOTHING;

-- Share Documents folder with Bob as editor
INSERT INTO shares (node_id, grantee_id, role) VALUES
    ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000002', 'editor')
ON CONFLICT DO NOTHING;

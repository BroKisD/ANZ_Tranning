package service_test

import (
	"context"
	"errors"
	"testing"

	"anz-training/model"
	"anz-training/service"
)

type fakeUserRepo struct {
	findErr   error // returned by FindUserByEmail
	insertErr error
	found     *model.User
	lastEmail string
	inserted  *model.User
}

func (r *fakeUserRepo) FindUserByEmail(_ context.Context, email string) (*model.User, error) {
	r.lastEmail = email
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.found, nil
}

func (r *fakeUserRepo) InsertUser(_ context.Context, u *model.User) error {
	r.inserted = u
	return r.insertErr
}

func TestUserService_Create(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name, email, userName string
		repo                  *fakeUserRepo
		wantErr               error
	}{
		{"empty email", "", "Ann", &fakeUserRepo{findErr: model.ErrNotFound}, model.ErrValidation},
		{"empty name", "a@b.co", "  ", &fakeUserRepo{findErr: model.ErrNotFound}, model.ErrValidation},
		{"invalid email", "nope", "Ann", &fakeUserRepo{findErr: model.ErrNotFound}, model.ErrValidation},
		{"email taken", "a@b.co", "Ann", &fakeUserRepo{found: &model.User{}}, model.ErrEmailTaken},
		{"lookup error", "a@b.co", "Ann", &fakeUserRepo{findErr: boom}, boom},
		{"insert error", "a@b.co", "Ann", &fakeUserRepo{findErr: model.ErrNotFound, insertErr: boom}, boom},
		{"success", " A@B.co ", " Ann ", &fakeUserRepo{findErr: model.ErrNotFound}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := service.NewUserService(tt.repo).Create(context.Background(), tt.email, tt.userName)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if u.Email != "a@b.co" || u.Name != "Ann" || u.ID == "" || tt.repo.inserted != u {
				t.Fatalf("created user = %+v", u)
			}
		})
	}
}

func TestUserService_GetByEmail_Normalizes(t *testing.T) {
	repo := &fakeUserRepo{found: &model.User{Email: "a@b.co"}}
	if _, err := service.NewUserService(repo).GetByEmail(context.Background(), "  A@B.CO "); err != nil {
		t.Fatal(err)
	}
	if repo.lastEmail != "a@b.co" {
		t.Fatalf("repo received %q", repo.lastEmail)
	}
}
